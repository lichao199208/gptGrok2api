package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func proxyTestConfig(root string) config.Config {
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.AccountsPath = root + "/accounts.json"
	cfg.AuthKeysPath = root + "/auth_keys.json"
	cfg.ConfigPath = root + "/config.json"
	return cfg
}

func TestProxyGroupTestPrunesFailedEnabledNodes(t *testing.T) {
	root := t.TempDir()
	server := New(proxyTestConfig(root))
	_, err := server.store.UpdateConfig("proxy_groups", []map[string]any{{
		"id":      "egress",
		"name":    "Egress",
		"enabled": true,
		"notes":   "keep this",
		"nodes": []map[string]any{
			{"id": "bad", "name": "Bad", "url": "http://[::1", "enabled": true},
			{"id": "disabled", "name": "Disabled", "url": "http://[::1", "enabled": false},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"id": "egress", "prune_failed": true})
	request := httptest.NewRequest(http.MethodPost, "/api/proxy/groups/test", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("group test returned %d: %s", response.Code, response.Body.String())
	}

	updated, err := server.store.Config()
	if err != nil {
		t.Fatal(err)
	}
	groups := mapList(updated["proxy_groups"])
	if len(groups) != 1 || stringValue(groups[0]["notes"]) != "keep this" {
		t.Fatalf("group metadata was not preserved: %#v", groups)
	}
	nodes := mapList(groups[0]["nodes"])
	if len(nodes) != 1 || stringValue(nodes[0]["id"]) != "disabled" {
		t.Fatalf("expected failed enabled node removed and disabled node retained: %#v", nodes)
	}
}

func TestProxyGroupSingleNodeTestNeverPrunes(t *testing.T) {
	root := t.TempDir()
	server := New(proxyTestConfig(root))
	_, err := server.store.UpdateConfig("proxy_groups", []map[string]any{{
		"id":    "egress",
		"nodes": []map[string]any{{"id": "bad", "url": "http://[::1", "enabled": true}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"id": "egress", "node_id": "bad", "prune_failed": true})
	request := httptest.NewRequest(http.MethodPost, "/api/proxy/groups/test", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("single node test returned %d: %s", response.Code, response.Body.String())
	}

	updated, err := server.store.Config()
	if err != nil {
		t.Fatal(err)
	}
	nodes := mapList(mapList(updated["proxy_groups"])[0]["nodes"])
	if len(nodes) != 1 || stringValue(nodes[0]["id"]) != "bad" {
		t.Fatalf("single-node test unexpectedly pruned node: %#v", nodes)
	}
}
