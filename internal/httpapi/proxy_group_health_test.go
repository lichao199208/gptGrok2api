package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestProxyGroupTestPersistsEachNodeAndRejectsHTTP403(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	server := New(cfg)
	server.proxyProbeURL = "http://health.test/probe"

	availableProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer availableProxy.Close()
	forbiddenProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer forbiddenProxy.Close()

	_, err := server.store.UpdateConfig("proxy_groups", []map[string]any{{
		"id": "group-health", "name": "health", "enabled": true,
		"nodes": []map[string]any{
			{"id": "node-ok", "name": "ok", "url": availableProxy.URL, "enabled": true},
			{"id": "node-403", "name": "forbidden", "url": forbiddenProxy.URL, "enabled": true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"id": "group-health"})
	response := adminRequest(http.HandlerFunc(server.proxyGroupTest), http.MethodPost, "/api/proxy/groups/test", bytes.NewReader(body))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	results := anyList(payload["results"])
	if len(results) != 2 {
		t.Fatalf("unexpected results: %#v", payload["results"])
	}
	byNode := map[string]map[string]any{}
	for _, raw := range results {
		item := mapValue(raw)
		byNode[stringValue(item["node_id"])] = mapValue(item["result"])
	}
	if !boolValue(byNode["node-ok"]["ok"], false) || intValue(byNode["node-ok"]["status"]) != http.StatusNoContent {
		t.Fatalf("available node was not healthy: %#v", byNode["node-ok"])
	}
	if boolValue(byNode["node-403"]["ok"], true) || intValue(byNode["node-403"]["status"]) != http.StatusForbidden || stringValue(byNode["node-403"]["error"]) != "HTTP 403" {
		t.Fatalf("HTTP 403 node was not rejected: %#v", byNode["node-403"])
	}

	stored, err := server.store.Config()
	if err != nil {
		t.Fatal(err)
	}
	groups := mapList(stored["proxy_groups"])
	nodes := mapList(groups[0]["nodes"])
	persisted := map[string]map[string]any{}
	for _, node := range nodes {
		persisted[stringValue(node["id"])] = node
	}
	if intValue(persisted["node-ok"]["last_status"]) != http.StatusNoContent || stringValue(persisted["node-ok"]["last_checked_at"]) == "" || stringValue(persisted["node-ok"]["last_error"]) != "" {
		t.Fatalf("healthy node status was not persisted: %#v", persisted["node-ok"])
	}
	if intValue(persisted["node-403"]["last_status"]) != http.StatusForbidden || stringValue(persisted["node-403"]["last_error"]) != "HTTP 403" || stringValue(persisted["node-403"]["last_error_at"]) == "" {
		t.Fatalf("failed node status was not persisted: %#v", persisted["node-403"])
	}
}

func TestProxyTestStatusOK(t *testing.T) {
	for _, test := range []struct {
		status int
		ok     bool
	}{{199, false}, {200, true}, {302, true}, {399, true}, {400, false}, {403, false}, {500, false}} {
		if got := proxyTestStatusOK(test.status); got != test.ok {
			t.Fatalf("status %d: got %v, want %v", test.status, got, test.ok)
		}
	}
}

func TestIntValueSupportsProbeLatency(t *testing.T) {
	if got := intValue(int64(808)); got != 808 {
		t.Fatalf("int64 latency: got %d, want 808", got)
	}
}
