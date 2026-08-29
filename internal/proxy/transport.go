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

func DialContext(ctx context.Context, target, proxyURL string) (net.Conn, error) {
	proxyURL = normalizeURL(proxyURL)
	if proxyURL == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", proxyURL)
	}
	if strings.HasPrefix(strings.ToLower(parsed.Scheme), "socks") {
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
}

func NewManager(single string, pool []string) *Manager {
	clean := make([]string, 0, len(pool))
	for _, item := range pool {
		if value := normalizeURL(item); value != "" {
			clean = append(clean, value)
		}
	}
	return &Manager{url: normalizeURL(single), pool: clean}
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
