package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerUsesAccountThenRotatesPool(t *testing.T) {
	manager := NewManager("http://single.invalid:8080", []string{"http://one.invalid:8080", "http://two.invalid:8080"})
	if got := manager.Resolve(map[string]any{"proxy": "socks5://account.invalid:1080"}, false); got != "socks5://account.invalid:1080" {
		t.Fatalf("account proxy was not preferred: %q", got)
	}
	if got := manager.Resolve(nil, false); got != "http://one.invalid:8080" {
		t.Fatalf("unexpected first pool proxy: %q", got)
	}
	if got := manager.Resolve(nil, false); got != "http://two.invalid:8080" {
		t.Fatalf("unexpected second pool proxy: %q", got)
	}
}

func TestImageGroupLeasesIncludeHTTP403AndKeepRequestAffinity(t *testing.T) {
	manager := NewManager("http://default.invalid:8080", nil)
	manager.ConfigureImageGroups("group:images", []GroupConfig{{
		ID: "images", Enabled: true, Nodes: []NodeConfig{
			{ID: "forbidden-probe", URL: "http://one.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1, LastStatus: http.StatusForbidden, LastError: "HTTP 403"},
			{ID: "healthy", URL: "http://two.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1, LastStatus: http.StatusNoContent},
			{ID: "dead", URL: "http://dead.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1, LastStatus: http.StatusBadGateway, LastError: "HTTP 502"},
		},
	}})

	first := manager.AcquireImage(nil)
	if first.NodeID != "forbidden-probe" || first.URL != "http://one.invalid:8080" {
		t.Fatalf("HTTP 403 node was not selected for runtime validation: %#v", first)
	}
	ctx := WithURL(context.Background(), first.URL)
	if selected, ok := URLSelectionFromContext(ctx); !ok || selected != first.URL {
		t.Fatalf("request-scoped proxy affinity was lost: %q, %v", selected, ok)
	}
	second := manager.AcquireImage(nil)
	if second.NodeID != "healthy" {
		t.Fatalf("second request did not rotate to the next node: %#v", second)
	}
	third := manager.AcquireImage(nil)
	if third.URL != "http://default.invalid:8080" || third.Source != "default" {
		t.Fatalf("default proxy was not retained as capacity fallback: %#v", third)
	}
	first.Release(false)
	second.Release(false)
	third.Release(false)
}

func TestImageGroupRuntimeFailureCoolsNodeAndSwitches(t *testing.T) {
	manager := NewManager("http://default.invalid:8080", nil)
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{
		{ID: "one", URL: "http://one.invalid:8080", Enabled: true, LastStatus: http.StatusForbidden},
		{ID: "two", URL: "http://two.invalid:8080", Enabled: true, LastStatus: http.StatusForbidden},
	}}})
	failed := manager.AcquireImage(nil)
	failed.Release(true)
	retry := manager.AcquireImage(nil)
	defer retry.Release(false)
	if failed.NodeID == retry.NodeID || retry.NodeID != "two" {
		t.Fatalf("retry did not switch away from cooled node: failed=%s retry=%s", failed.NodeID, retry.NodeID)
	}
}

func TestImageGroupRemovesNodeAfterThreeConsecutiveRuntimeFailures(t *testing.T) {
	manager := NewManager("http://default.invalid:8080", nil)
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{
		{ID: "bad", URL: "http://bad.invalid:8080", Enabled: true},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, RuntimeSuccesses: 3},
	}}})
	events := []ImageNodeRuntimeResult{}
	manager.SetImageNodeResultCallback(func(event ImageNodeRuntimeResult) { events = append(events, event) })
	manager.mu.Lock()
	manager.imageGroups["images"].nodes[1].cooldownUntil = time.Now().Add(time.Hour)
	manager.mu.Unlock()
	for failures := 1; failures <= 3; failures++ {
		lease := manager.acquireGroup("images")
		if lease == nil || lease.NodeID != "bad" {
			t.Fatalf("failure %d did not acquire target node: %#v", failures, lease)
		}
		lease.Release(true)
		manager.mu.Lock()
		if failures < 3 {
			manager.imageGroups["images"].nodes[0].cooldownUntil = time.Time{}
		}
		manager.mu.Unlock()
	}
	if len(events) != 3 || !events[2].Removed || events[2].Failures != 3 {
		t.Fatalf("unexpected runtime events: %#v", events)
	}
	manager.mu.Lock()
	nodes := manager.imageGroups["images"].nodes
	manager.mu.Unlock()
	if len(nodes) != 1 || nodes[0].id != "stable" {
		t.Fatalf("failed node remained in runtime group: %#v", nodes)
	}
}

func TestAcquireStableImageRequiresThreeSuccessfulRequests(t *testing.T) {
	manager := NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{
		{ID: "new", URL: "http://new.invalid:8080", Enabled: true, RuntimeSuccesses: 2},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, RuntimeSuccesses: 3},
	}}})
	lease := manager.AcquireStableImage(nil, "")
	if lease == nil || lease.NodeID != "stable" {
		t.Fatalf("expected stable node, got %#v", lease)
	}
	lease.Release(false)
}

func TestHTTPProxyTransport(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("target-ok")) }))
	defer target.Close()
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.String(), target.URL) {
			t.Errorf("proxy did not receive absolute target URL: %q", r.URL.String())
		}
		response, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = fmt.Fprint(w, "target-ok")
	}))
	defer proxyServer.Close()
	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	request, _ := http.NewRequestWithContext(WithURL(context.Background(), proxyServer.URL), http.MethodGet, target.URL, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
}

func TestSOCKS4aConnectUsesDomainName(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	packetCh := make(chan []byte, 1)
	go func() {
		packet := make([]byte, 9+len("example.com")+1)
		_, _ = io.ReadFull(server, packet)
		packetCh <- packet
		_, _ = server.Write([]byte{0x00, 0x5a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	}()
	if err := socks4Connect(client, "example.com:443", nil); err != nil {
		t.Fatal(err)
	}
	packet := <-packetCh
	if len(packet) < 10 || packet[0] != 0x04 || packet[1] != 0x01 || packet[2] != 0x01 || packet[3] != 0xbb {
		t.Fatalf("unexpected SOCKS4 request header: %x", packet)
	}
	if got := string(packet[9 : len(packet)-1]); got != "example.com" {
		t.Fatalf("unexpected SOCKS4a domain: %q", got)
	}
}

func TestUpstreamRouterReadsFileAndNormalizesEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstreams.txt")
	content := "# comment\nhttp://user:pass@one.invalid:8080\n\none.invalid:8080\nhttps://two.invalid\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	router := NewUpstreamRouter(path)
	if router == nil {
		t.Fatal("router was nil")
	}
	router.mu.Lock()
	router.entries = []upstreamEntry{
		{URL: "http://user:pass@one.invalid:8080", Healthy: true, LastChecked: time.Now()},
		{URL: "http://one.invalid:8080", Healthy: true, LastChecked: time.Now()},
		{URL: "https://two.invalid", Healthy: true, LastChecked: time.Now()},
	}
	router.mu.Unlock()
	if got := router.Resolve(); got == "" {
		t.Fatal("expected upstream url")
	}
	snapshot := router.Snapshot()
	if count := snapshot["count"].(int); count != 3 {
		t.Fatalf("unexpected snapshot count: %v", count)
	}
}
