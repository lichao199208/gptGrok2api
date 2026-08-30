package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

// WithURL attaches one egress proxy to a request. An empty URL means direct.
func WithURL(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, contextKey{}, strings.TrimSpace(value))
}

func URLFromContext(ctx context.Context) string {
	value, _ := ctx.Value(contextKey{}).(string)
	return strings.TrimSpace(value)
}

func URLSelectionFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(contextKey{}).(string)
	return strings.TrimSpace(value), ok
}

func DialContext(ctx context.Context, target, proxyURL string) (net.Conn, error) {
	proxyURL = normalizeURL(proxyURL)
	if proxyURL == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", proxyURL)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks4", "socks4a":
		return (&socks4Dialer{proxy: parsed}).DialContext(ctx, "tcp", target)
	case "socks", "socks5", "socks5h":
		return (&socks5Dialer{proxy: parsed}).DialContext(ctx, "tcp", target)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	proxyAddress := parsed.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "8080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	connect := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(parsed.User.Username() + ":" + password))
		connect += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	connect += "\r\n"
	if _, err := io.WriteString(conn, connect); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP proxy CONNECT returned HTTP %d", response.StatusCode)
	}
	return conn, nil
}

// Manager resolves an account-specific proxy first, then rotates the global pool.
type Manager struct {
	mu             sync.Mutex
	url            string
	pool           []string
	cursor         int
	resourceURL    string
	resourcePool   []string
	resourceCursor int
	upstreamRouter *UpstreamRouter
	imageGroups    map[string]*imageGroup
	imageGroupID   string
	imageCursor    int
	onImageResult  func(ImageNodeRuntimeResult)
}

type GroupConfig struct {
	ID, Name, Strategy string
	Enabled            bool
	Nodes              []NodeConfig
}

type NodeConfig struct {
	ID, Name, URL         string
	Enabled               bool
	ImageConcurrencyLimit int
	LastStatus            int
	LastError             string
	RuntimeFailures       int
	RuntimeSuccesses      int
}

type imageGroup struct {
	id, name, strategy string
	nodes              []*imageNode
}

type imageNode struct {
	id, name, url                        string
	limit, inFlight, failures, successes int
	cooldownUntil                        time.Time
	evicted                              bool
}

type Lease struct {
	manager   *Manager
	node      *imageNode
	URL       string
	Source    string
	GroupID   string
	GroupName string
	NodeID    string
	NodeName  string
	once      sync.Once
}

type EgressInfo struct {
	Source, GroupID, GroupName, NodeID, NodeName string
}

type ImageNodeRuntimeResult struct {
	GroupID, GroupName, NodeID, NodeName, URL string
	Failures, Successes                       int
	Removed                                   bool
}

const (
	imageNodeFailureLimit  = 3
	imageNodeStableSuccess = 3
)

func (m *Manager) SetImageNodeResultCallback(callback func(ImageNodeRuntimeResult)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onImageResult = callback
	m.mu.Unlock()
}

func NewManager(single string, pool []string) *Manager {
	clean := make([]string, 0, len(pool))
	for _, item := range pool {
		if value := normalizeURL(item); value != "" {
			clean = append(clean, value)
		}
	}
	return &Manager{url: normalizeURL(single), pool: clean, imageGroups: map[string]*imageGroup{}}
}

// ConfigureImageGroups enables request-scoped image egress selection. A
// fallback group is used as the active image pool; the default proxy remains
// available when every group node is busy or cooling down.
func (m *Manager) ConfigureImageGroups(fallback string, groups []GroupConfig) {
	if m == nil {
		return
	}
	next := map[string]*imageGroup{}
	for _, group := range groups {
		id := strings.TrimSpace(group.ID)
		if id == "" || !group.Enabled {
			continue
		}
		item := &imageGroup{id: id, name: strings.TrimSpace(group.Name), strategy: strings.TrimSpace(group.Strategy)}
		for _, node := range group.Nodes {
			proxyURL := normalizeURL(node.URL)
			if !node.Enabled || proxyURL == "" || !probeAllowsRuntimeValidation(node.LastStatus, node.LastError) {
				continue
			}
			limit := node.ImageConcurrencyLimit
			if limit < 1 {
				limit = 20
			}
			failures := node.RuntimeFailures
			if failures < 0 {
				failures = 0
			}
			if failures >= imageNodeFailureLimit {
				continue
			}
			successes := node.RuntimeSuccesses
			if successes < 0 {
				successes = 0
			}
			item.nodes = append(item.nodes, &imageNode{id: strings.TrimSpace(node.ID), name: strings.TrimSpace(node.Name), url: proxyURL, limit: limit, failures: failures, successes: successes})
		}
		next[id] = item
	}
	groupID := ""
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(fallback)), "group:") {
		groupID = strings.TrimSpace(strings.TrimSpace(fallback)[len("group:"):])
	}
	m.mu.Lock()
	m.imageGroups = next
	m.imageGroupID = groupID
	m.imageCursor = 0
	m.mu.Unlock()
}

func probeAllowsRuntimeValidation(status int, lastError string) bool {
	if status == http.StatusForbidden || (status >= 200 && status < 400) {
		return true
	}
	return status == 0 && strings.TrimSpace(lastError) == ""
}

// AcquireImage chooses one proxy for the complete multi-stage image request.
// The caller must release the lease so node concurrency and cooldown state stay accurate.
func (m *Manager) AcquireImage(fields map[string]any) *Lease {
	if m == nil {
		return &Lease{}
	}
	if fields != nil {
		for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
			value := strings.TrimSpace(stringValue(fields[key]))
			if value == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(value), "group:") {
				if lease := m.acquireGroup(strings.TrimSpace(value[len("group:"):])); lease != nil {
					return lease
				}
				break
			}
			if normalized := normalizeURL(value); normalized != "" {
				return &Lease{URL: normalized, Source: "account"}
			}
		}
	}
	m.mu.Lock()
	groupID := m.imageGroupID
	m.mu.Unlock()
	if groupID != "" {
		if lease := m.acquireGroup(groupID); lease != nil {
			return lease
		}
	}
	return &Lease{URL: m.Resolve(nil, false), Source: "default"}
}

func (m *Manager) acquireGroup(groupID string) *Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	group := m.imageGroups[groupID]
	if group == nil || len(group.nodes) == 0 {
		return nil
	}
	now := time.Now()
	for offset := 0; offset < len(group.nodes); offset++ {
		index := (m.imageCursor + offset) % len(group.nodes)
		node := group.nodes[index]
		if node.inFlight >= node.limit || now.Before(node.cooldownUntil) {
			continue
		}
		node.inFlight++
		m.imageCursor = (index + 1) % len(group.nodes)
		return &Lease{manager: m, node: node, URL: node.url, Source: "group", GroupID: group.id, GroupName: group.name, NodeID: node.id, NodeName: node.name}
	}
	return nil
}

// AcquireStableImage selects a different group node that has already
// completed several real image requests. It never falls back to direct mode
// or the default proxy.
func (m *Manager) AcquireStableImage(fields map[string]any, excludedURL string) *Lease {
	if m == nil {
		return nil
	}
	groupID := ""
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		value := strings.TrimSpace(stringValue(fields[key]))
		if strings.HasPrefix(strings.ToLower(value), "group:") {
			groupID = strings.TrimSpace(value[len("group:"):])
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if groupID == "" {
		groupID = m.imageGroupID
	}
	group := m.imageGroups[groupID]
	if group == nil || len(group.nodes) == 0 {
		return nil
	}
	now := time.Now()
	excludedURL = normalizeURL(excludedURL)
	for offset := 0; offset < len(group.nodes); offset++ {
		index := (m.imageCursor + offset) % len(group.nodes)
		node := group.nodes[index]
		if node.evicted || node.url == excludedURL || node.successes < imageNodeStableSuccess || node.failures > 0 ||
			node.inFlight >= node.limit || now.Before(node.cooldownUntil) {
			continue
		}
		node.inFlight++
		m.imageCursor = (index + 1) % len(group.nodes)
		return &Lease{manager: m, node: node, URL: node.url, Source: "group", GroupID: group.id, GroupName: group.name, NodeID: node.id, NodeName: node.name}
	}
	return nil
}

func (l *Lease) Release(runtimeFailure bool) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.manager == nil || l.node == nil {
			return
		}
		var event *ImageNodeRuntimeResult
		l.manager.mu.Lock()
		if l.node.inFlight > 0 {
			l.node.inFlight--
		}
		if runtimeFailure && !l.node.evicted {
			l.node.failures++
			cooldown := time.Duration(1<<min(l.node.failures-1, 4)) * time.Minute
			l.node.cooldownUntil = time.Now().Add(cooldown)
			removed := l.node.failures >= imageNodeFailureLimit
			if removed {
				l.node.evicted = true
				if group := l.manager.imageGroups[l.GroupID]; group != nil {
					nodes := make([]*imageNode, 0, len(group.nodes)-1)
					for _, node := range group.nodes {
						if node != l.node {
							nodes = append(nodes, node)
						}
					}
					group.nodes = nodes
					if len(nodes) == 0 {
						l.manager.imageCursor = 0
					} else {
						l.manager.imageCursor %= len(nodes)
					}
				}
			}
			event = &ImageNodeRuntimeResult{GroupID: l.GroupID, GroupName: l.GroupName, NodeID: l.NodeID, NodeName: l.NodeName, URL: l.URL, Failures: l.node.failures, Successes: l.node.successes, Removed: removed}
		} else if !runtimeFailure && !l.node.evicted {
			hadFailures := l.node.failures > 0
			l.node.failures = 0
			l.node.cooldownUntil = time.Time{}
			l.node.successes++
			if hadFailures || l.node.successes == imageNodeStableSuccess || l.node.successes%25 == 0 {
				event = &ImageNodeRuntimeResult{GroupID: l.GroupID, GroupName: l.GroupName, NodeID: l.NodeID, NodeName: l.NodeName, URL: l.URL, Successes: l.node.successes}
			}
		}
		callback := l.manager.onImageResult
		if event != nil && callback != nil {
			callback(*event)
		}
		l.manager.mu.Unlock()
	})
}

func (m *Manager) DescribeImageEgress(proxyURL string) EgressInfo {
	if m == nil {
		return EgressInfo{Source: "direct"}
	}
	normalized := normalizeURL(proxyURL)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, group := range m.imageGroups {
		for _, node := range group.nodes {
			if node.url == normalized {
				return EgressInfo{Source: "group", GroupID: group.id, GroupName: group.name, NodeID: node.id, NodeName: node.name}
			}
		}
	}
	if normalized == "" {
		return EgressInfo{Source: "direct"}
	}
	return EgressInfo{Source: "default"}
}

func (m *Manager) SetResource(single string, pool []string) {
	if m == nil {
		return
	}
	clean := make([]string, 0, len(pool))
	for _, item := range pool {
		if value := normalizeURL(item); value != "" {
			clean = append(clean, value)
		}
	}
	m.mu.Lock()
	m.resourceURL = normalizeURL(single)
	m.resourcePool = clean
	m.resourceCursor = 0
	m.mu.Unlock()
}

func (m *Manager) SetUpstreamsFile(path string) {
	if m == nil {
		return
	}
	router := NewUpstreamRouter(path)
	m.mu.Lock()
	m.upstreamRouter = router
	m.mu.Unlock()
}

func (m *Manager) Resolve(fields map[string]any, resource bool) string {
	if fields != nil {
		for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
			if value := stringValue(fields[key]); value != "" && !strings.HasPrefix(strings.ToLower(value), "group:") {
				if normalized := normalizeURL(value); normalized != "" {
					return normalized
				}
			}
		}
	}
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pool := m.pool
	baseURL := m.url
	if resource && len(m.resourcePool) > 0 {
		pool = m.resourcePool
		baseURL = m.resourceURL
	}
	if len(pool) > 0 {
		cursor := &m.cursor
		if resource {
			cursor = &m.resourceCursor
		}
		value := pool[*cursor%len(pool)]
		*cursor++
		return value
	}
	if resource && m.resourceURL != "" {
		return m.resourceURL
	}
	if m.upstreamRouter != nil {
		if upstream := m.upstreamRouter.Resolve(); upstream != "" {
			return upstream
		}
	}
	return baseURL
}

func (m *Manager) Snapshot() map[string]any {
	if m == nil {
		return map[string]any{"mode": "direct", "count": 0}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mode := "direct"
	if len(m.pool) > 0 {
		mode = "proxy_pool"
	} else if m.url != "" {
		mode = "single_proxy"
	}
	snapshot := map[string]any{"mode": mode, "count": len(m.pool), "resource_count": len(m.resourcePool), "proxy_configured": m.url != "" || len(m.pool) > 0 || m.resourceURL != "" || len(m.resourcePool) > 0}
	if group := m.imageGroups[m.imageGroupID]; group != nil {
		snapshot["mode"] = "proxy_group"
		snapshot["image_group_id"] = group.id
		snapshot["image_group_count"] = len(group.nodes)
		snapshot["proxy_configured"] = len(group.nodes) > 0 || snapshot["proxy_configured"] == true
	}
	if m.upstreamRouter != nil {
		snapshot["upstreams"] = m.upstreamRouter.Snapshot()
	}
	return snapshot
}

// Transport selects a transport per request so account proxy affinity does not
// require rebuilding all providers. HTTP(S) proxies use net/http; SOCKS5 is
// implemented locally to keep the main Go module dependency-free.
type Transport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	cache map[string]http.RoundTripper
}

func NewTransport(base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, cache: map[string]http.RoundTripper{"": base}}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxyURL := URLFromContext(req.Context())
	roundTripper, err := t.forProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	return roundTripper.RoundTrip(req)
}

func (t *Transport) forProxy(proxyURL string) (http.RoundTripper, error) {
	proxyURL = normalizeURL(proxyURL)
	t.mu.Lock()
	if roundTripper, ok := t.cache[proxyURL]; ok {
		t.mu.Unlock()
		return roundTripper, nil
	}
	t.mu.Unlock()
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", proxyURL)
	}
	var roundTripper http.RoundTripper
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = http.ProxyURL(parsed)
		roundTripper = transport
	case "socks4", "socks4a":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = nil
		transport.DialContext = (&socks4Dialer{proxy: parsed}).DialContext
		roundTripper = transport
	case "socks5", "socks5h", "socks":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = nil
		transport.DialContext = (&socks5Dialer{proxy: parsed}).DialContext
		roundTripper = transport
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	t.mu.Lock()
	if current, exists := t.cache[proxyURL]; exists {
		roundTripper = current
	} else {
		t.cache[proxyURL] = roundTripper
	}
	t.mu.Unlock()
	return roundTripper, nil
}

func cloneHTTPTransport(base http.RoundTripper) *http.Transport {
	if transport, ok := base.(*http.Transport); ok {
		return transport.Clone()
	}
	return http.DefaultTransport.(*http.Transport).Clone()
}

type socks5Dialer struct{ proxy *url.URL }

type socks4Dialer struct{ proxy *url.URL }

func (d *socks4Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS4 only supports TCP, got %s", network)
	}
	proxyAddress := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "1080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if err := socks4Connect(conn, address, d.proxy.User); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks4Connect(conn io.ReadWriter, address string, user *url.Userinfo) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portText)
	}
	packet := []byte{0x04, 0x01, byte(port >> 8), byte(port)}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		packet = append(packet, 0x00, 0x00, 0x00, 0x01)
	} else {
		packet = append(packet, ip...)
	}
	if user != nil {
		packet = append(packet, []byte(user.Username())...)
	}
	packet = append(packet, 0x00)
	if ip == nil {
		packet = append(packet, []byte(host)...)
		packet = append(packet, 0x00)
	}
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[1] != 0x5a {
		return fmt.Errorf("SOCKS4 connect failed with code 0x%02x", reply[1])
	}
	return nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 only supports TCP, got %s", network)
	}
	proxyAddress := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "1080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if err := socks5Handshake(conn, d.proxy.User); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := socks5Connect(conn, address); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks5Handshake(conn io.ReadWriter, user *url.Userinfo) error {
	methods := []byte{0x00}
	if user != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var selected [2]byte
	if _, err := io.ReadFull(conn, selected[:]); err != nil {
		return err
	}
	if selected[0] != 0x05 {
		return errors.New("invalid SOCKS5 version")
	}
	if selected[1] == 0xff {
		return errors.New("SOCKS5 proxy rejected authentication methods")
	}
	if selected[1] == 0x02 {
		if user == nil {
			return errors.New("SOCKS5 requested username authentication")
		}
		username := user.Username()
		password, _ := user.Password()
		if len(username) > 255 || len(password) > 255 {
			return errors.New("SOCKS5 credentials are too long")
		}
		packet := append([]byte{0x01, byte(len(username))}, []byte(username)...)
		packet = append(packet, byte(len(password)))
		packet = append(packet, []byte(password)...)
		if _, err := conn.Write(packet); err != nil {
			return err
		}
		var authReply [2]byte
		if _, err := io.ReadFull(conn, authReply[:]); err != nil {
			return err
		}
		if authReply[1] != 0x00 {
			return errors.New("SOCKS5 username authentication failed")
		}
	}
	return nil
}

func socks5Connect(conn io.ReadWriter, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portText)
	}
	packet := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			packet = append(packet, 0x01)
			packet = append(packet, ip4...)
		} else {
			packet = append(packet, 0x04)
			packet = append(packet, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS5 target hostname is too long")
		}
		packet = append(packet, 0x03, byte(len(host)))
		packet = append(packet, []byte(host)...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	packet = append(packet, portBytes[:]...)
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with code 0x%02x", reply[1])
	}
	length := 0
	switch reply[3] {
	case 0x01:
		length = 4
	case 0x04:
		length = 16
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		length = int(size[0])
	default:
		return errors.New("SOCKS5 returned an invalid address type")
	}
	discard := make([]byte, length+2)
	_, err = io.ReadFull(conn, discard)
	return err
}

func normalizeURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "direct") {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
