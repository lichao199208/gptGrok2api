package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGPTMailPublicStatusAndKeyRefreshRedactKey(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/public-key-status" || r.Header.Get("X-Public-Key-Reveal") != "click" || r.URL.Query().Get("reveal") != "1" {
			t.Fatalf("unexpected public status request: %s headers=%v", r.URL.String(), r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"is_active": true, "daily_limit": 100, "used_today": 12, "remaining_today": 88, "key": "public-secret-key"}})
	}))
	defer server.Close()

	client := NewGPTMail(server.Client())
	config := map[string]any{"api_base": server.URL, "key_mode": "public", "default_domain": "example.test"}
	result, err := client.Status(context.Background(), config, false)
	if err != nil {
		t.Fatal(err)
	}
	if result["remaining_today"] != float64(88) || result["key_hint"] != "publi...-key" || result["api_key"] != nil {
		t.Fatalf("unexpected redacted status: %#v", result)
	}
	_, err = client.Status(context.Background(), config, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected cached status, got %d upstream calls", calls)
	}
	refreshed, err := client.RefreshPublicKey(context.Background(), config, true)
	if err != nil || refreshed["key_hint"] != "publi...-key" {
		t.Fatalf("unexpected refreshed status: %#v %v", refreshed, err)
	}
}

func TestGPTMailCustomStatusUsesAPIKeyAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" || r.Header.Get("X-API-Key") != "custom-secret" {
			t.Fatalf("unexpected custom request: %s key=%q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "usage": map[string]any{"daily_limit": 50, "used_today": 7, "remaining_today": 43, "total_limit": 500}})
	}))
	defer server.Close()
	result, err := NewGPTMail(server.Client()).Status(context.Background(), map[string]any{"api_base": server.URL, "api_key": "custom-secret"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result["source"] != "stats" || result["remaining_today"] != float64(43) || result["key_hint"] != "custo...cret" {
		t.Fatalf("unexpected custom result: %#v", result)
	}
}
