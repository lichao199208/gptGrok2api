package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGrokQuotaRefreshFetchesAllModesAndParsesWindows(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mode := body["modelName"]
		mu.Lock()
		seen[mode] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"remainingQueries": 4, "totalQueries": 10, "windowSizeSeconds": 3600})
	}))
	defer server.Close()

	quota := NewGrokQuota(server.URL, server.Client(), nil)
	values, err := quota.RefreshToken(context.Background(), "sso-live-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 5 {
		t.Fatalf("expected five quota modes, got %#v", values)
	}
	for _, mode := range []string{"auto", "fast", "expert", "heavy", "grok_4_3"} {
		window, ok := values[mode]
		if !ok || window.Remaining != 4 || window.Total != 10 || window.WindowSeconds != 3600 || window.ResetAt <= 0 {
			t.Fatalf("unexpected %s window: %#v", mode, window)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("expected all mode names, got %#v", seen)
	}
}

func TestGrokQuotaClassifiesInvalidCredentialMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid-credentials", http.StatusBadRequest)
	}))
	defer server.Close()

	quota := NewGrokQuota(server.URL, server.Client(), nil)
	_, err := quota.ProbeFast(context.Background(), "bad-token", nil)
	if err == nil || err != ErrGrokInvalidCredentials {
		t.Fatalf("expected invalid credential classification, got %v", err)
	}
}
