package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestXAIProbeRefreshesAfterUnauthorizedAndReadsBilling(t *testing.T) {
	responseCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			responseCalls++
			if r.Header.Get("Authorization") == "Bearer old-token" {
				http.Error(w, `{"error":{"code":"invalid_token","message":"expired"}}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-1", "usage": map[string]any{"total_tokens": 1}})
		case "/billing":
			_ = json.NewEncoder(w).Encode(map[string]any{"creditUsagePercent": 23.5})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token", "refresh_token": "new-refresh", "id_token": "new-id"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := NewXAIProbe(server.URL, server.URL+"/token", server.Client()).Probe(context.Background(), map[string]any{"access_token": "old-token", "refresh_token": "refresh"})
	if result.Status != "valid" || result.AccessToken != "new-token" || result.RefreshToken != "new-refresh" || responseCalls != 2 {
		t.Fatalf("unexpected probe result: %#v calls=%d", result, responseCalls)
	}
	if result.Quota["used_percent"] != 23.5 || result.Quota["remaining_percent"] != 76.5 {
		t.Fatalf("unexpected quota: %#v", result.Quota)
	}
}
