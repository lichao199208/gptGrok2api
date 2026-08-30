package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProxySubscriptionSupportsPlainAndBase64Lists(t *testing.T) {
	plain := parseProxySubscription("# comment\nhttp://127.0.0.1:8080\nsocks5h://user:pass@example.test:1080\ninvalid")
	if len(plain) != 2 || plain[1] != "socks5://user:pass@example.test:1080" {
		t.Fatalf("unexpected plain subscription: %#v", plain)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("http://127.0.0.1:8080\nhttps://example.test:443"))
	decoded := parseProxySubscription(encoded)
	if len(decoded) != 2 || decoded[0] != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected base64 subscription: %#v", decoded)
	}
}

func TestRefreshProxyGroupSubscriptionImportsNodesAndPreservesManualNodes(t *testing.T) {
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "GPTGrok2API-ProxySubscription/1.0" {
			t.Fatalf("unexpected user agent: %q", got)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("http://127.0.0.1:8080\nsocks5://127.0.0.1:1080\n"))
	}))
	defer subscription.Close()

	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.AccountsPath = root + "/accounts.json"
	cfg.AuthKeysPath = root + "/auth_keys.json"
	cfg.ConfigPath = root + "/config.json"
	server := New(cfg)
	config := map[string]any{
		"proxy_groups": []map[string]any{{
			"id":               "test-group",
			"name":             "Test group",
			"subscription_url": subscription.URL,
			"subscription_node_image_concurrency_limit": 7,
			"nodes": []any{
				map[string]any{"id": "manual", "url": "http://manual.example:8080"},
				map[string]any{"id": "old-sub", "url": "http://old.example:8080", "source": "subscription", "subscription_managed": true},
			},
		}},
	}
	if err := server.store.ReplaceConfig(config); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/proxy/groups/test-group/subscription/refresh", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("refresh returned %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"node_count":2`) {
		t.Fatalf("missing node count: %s", response.Body.String())
	}

	updated, err := server.store.Config()
	if err != nil {
		t.Fatal(err)
	}
	groups := mapList(updated["proxy_groups"])
	nodes := mapList(groups[0]["nodes"])
	if len(nodes) != 3 || stringValue(nodes[0]["id"]) != "manual" || stringValue(nodes[1]["source"]) != "subscription" {
		t.Fatalf("manual/subscription nodes were not merged correctly: %#v", nodes)
	}
	if intValue(nodes[1]["image_concurrency_limit"]) != 7 {
		t.Fatalf("subscription concurrency was not preserved: %#v", nodes[1])
	}
}
