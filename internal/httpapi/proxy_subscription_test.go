package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestParseProxySubscriptionPlainTextAndBase64(t *testing.T) {
	plain := strings.Join([]string{
		"# proxies",
		"http://Example.COM:8080",
		"http://example.com:8080",
		"socks4://127.0.0.1:1080",
		"socks5://user:pass@proxy.example:1080",
		"host.example:3128",
		"not a proxy",
	}, "\n")
	expected := []string{
		"http://example.com:8080",
		"socks4://127.0.0.1:1080",
		"socks5://user:pass@proxy.example:1080",
		"http://host.example:3128",
	}
	for name, raw := range map[string][]byte{
		"plain":  []byte(plain),
		"base64": []byte(base64.StdEncoding.EncodeToString([]byte(plain))),
	} {
		t.Run(name, func(t *testing.T) {
			nodes, err := parseProxySubscription(raw)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(nodes, "\n") != strings.Join(expected, "\n") {
				t.Fatalf("unexpected nodes: %#v", nodes)
			}
		})
	}
}

func TestParseProxySubscriptionRejectsMoreThanLimit(t *testing.T) {
	var input strings.Builder
	for index := 0; index <= maxProxySubscriptionNodes; index++ {
		input.WriteString("http://proxy-")
		input.WriteString(strconv.Itoa(index))
		input.WriteString(":8080\n")
	}
	_, err := parseProxySubscription([]byte(input.String()))
	if err == nil || !strings.Contains(err.Error(), "5000") {
		t.Fatalf("expected node limit error, got %v", err)
	}
}

func TestRefreshProxyGroupSubscriptionPreservesConcurrentAdminChanges(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(strings.Join([]string{
			"http://manual.example:8080",
			"socks4://subscription-one.example:1080",
			"socks5://subscription-two.example:1080",
			"socks5://subscription-two.example:1080",
		}, "\n")))
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	server.requestClient = upstream.Client()
	group := map[string]any{
		"id": "group-one", "name": "Original", "subscription_url": upstream.URL,
		"subscription_node_image_concurrency_limit": 20,
		"nodes": []any{
			map[string]any{"id": "manual-one", "url": "http://manual.example:8080", "enabled": true},
			map[string]any{"id": "old-subscription", "url": "http://old.example:8080", "subscription_managed": true},
		},
	}
	if _, err := server.store.UpdateConfig("proxy_groups", []any{group}); err != nil {
		t.Fatal(err)
	}

	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- adminRequest(server.Handler(), http.MethodPost, "/api/proxy/groups/group-one/subscription/refresh", nil)
	}()
	<-started
	if _, err := server.store.MutateConfig("proxy_groups", func(value any) (any, error) {
		groups := mapList(value)
		next := cloneMap(groups[0])
		next["name"] = "Changed by another admin"
		nodes := mapList(next["nodes"])
		nodes = append(nodes, map[string]any{"id": "manual-two", "url": "http://second-manual.example:8080", "enabled": true})
		next["nodes"] = nodes
		groups[0] = next
		return groups, nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	response := <-responseCh
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if intValue(payload["node_count"]) != 2 {
		t.Fatalf("unexpected subscription count: %#v", payload)
	}
	updated := mapValue(payload["group"])
	if stringValue(updated["name"]) != "Changed by another admin" {
		t.Fatalf("concurrent admin change was overwritten: %#v", updated)
	}
	nodes := mapList(updated["nodes"])
	if len(nodes) != 4 {
		t.Fatalf("expected two manual and two subscription nodes, got %#v", nodes)
	}
	manual, subscription := 0, 0
	for _, node := range nodes {
		if isSubscriptionProxyNode(node) {
			subscription++
		} else {
			manual++
		}
	}
	if manual != 2 || subscription != 2 {
		t.Fatalf("unexpected node ownership: manual=%d subscription=%d", manual, subscription)
	}
}

func TestRefreshProxyGroupSubscriptionRecordsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	server.requestClient = upstream.Client()
	group := map[string]any{
		"id": "group-failure", "subscription_url": upstream.URL,
		"nodes": []any{map[string]any{"id": "old", "url": "http://old.example:8080", "subscription_managed": true}},
	}
	if _, err := server.store.UpdateConfig("proxy_groups", []any{group}); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(server.Handler(), http.MethodPost, "/api/proxy/groups/group-failure/subscription/refresh", nil)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	updated, err := server.proxyGroupConfig("group-failure")
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(updated["subscription_last_attempt_at"]) == "" || !strings.Contains(stringValue(updated["subscription_last_error"]), "503") {
		t.Fatalf("subscription failure was not recorded: %#v", updated)
	}
	if len(mapList(updated["nodes"])) != 1 {
		t.Fatalf("failed refresh replaced existing nodes: %#v", updated)
	}
}
