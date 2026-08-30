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
