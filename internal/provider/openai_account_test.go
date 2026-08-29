package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestOpenAIAccountClientFetchesQuotaAndPlanConcurrently(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "user@example.test", "id": "user-1"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model_slug": "gpt-5",
				"limits_progress":    []any{map[string]any{"feature_name": "image_gen", "remaining": 7, "reset_after": "2026-08-25T00:00:00Z"}},
			})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIAccountClient(server.URL, server.URL, server.Client(), nil)
	result, err := client.RefreshAccount(context.Background(), map[string]any{"access_token": "access-token", "source_type": "chatgpt_web"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fields["email"] != "user@example.test" || result.Fields["user_id"] != "user-1" {
		t.Fatalf("unexpected identity fields: %#v", result.Fields)
	}
	if result.Fields["quota"] != 7 || result.Fields["type"] != "plus" || result.Fields["status"] != "正常" {
		t.Fatalf("unexpected quota fields: %#v", result.Fields)
	}
	if len(seen) != 3 {
		t.Fatalf("expected all account endpoints, got %#v", seen)
	}
}

func TestOpenAIAccountClientRefreshesOAuthAfter401(t *testing.T) {
	var mu sync.Mutex
	oldCalls, newCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "refresh-token" || r.Form.Get("client_id") != openAIOAuthClientID {
				t.Fatalf("unexpected oauth form: %s", formString(r))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "id_token": "new-id"})
			return
		}
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		if authorization == "Bearer old-access" {
			oldCalls++
		} else if authorization == "Bearer new-access" {
			newCalls++
		}
		mu.Unlock()
		if authorization == "Bearer old-access" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		if authorization != "Bearer new-access" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "rotated@example.test", "id": "user-2"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 2}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIAccountClient(server.URL, server.URL+"/oauth/token", server.Client(), nil)
	result, err := client.RefreshAccount(context.Background(), map[string]any{
		"access_token": "old-access", "refresh_token": "refresh-token", "source_type": "oauth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "new-access" || result.RefreshToken != "new-refresh" || result.IDToken != "new-id" {
		t.Fatalf("token rotation was not returned: %#v", result)
	}
	if result.Fields["email"] != "rotated@example.test" || result.Fields["quota"] != 2 {
		t.Fatalf("unexpected refreshed fields: %#v", result.Fields)
	}
	if oldCalls != 1 || newCalls != 3 {
		t.Fatalf("expected 1 old and 3 new endpoint calls, got old=%d new=%d", oldCalls, newCalls)
	}
}

func TestOpenAIAccountClientUsesFlareSolverrAfterCloudflare403(t *testing.T) {
	var mu sync.Mutex
	flareCalls := 0
	chatCalls := 0
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		flareCalls++
		mu.Unlock()
		if r.URL.Path != "/v1" || r.Method != http.MethodPost {
			t.Fatalf("unexpected FlareSolverr request: %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["cmd"] != "request.get" && payload["cmd"] != "request.post" {
			t.Fatalf("unexpected FlareSolverr payload: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"solution": map[string]any{
				"userAgent": "solver-browser",
				"cookies":   []map[string]string{{"name": "cf_clearance", "value": "clearance-value"}},
			},
		})
	}))
	defer flare.Close()

	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		chatCalls++
		mu.Unlock()
		if r.Header.Get("Cookie") != "cf_clearance=clearance-value" || r.Header.Get("User-Agent") != "solver-browser" {
			http.Error(w, "missing clearance", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "clearance@example.test", "id": "user-clearance"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 4}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer chat.Close()

	client := NewOpenAIAccountClient(chat.URL, chat.URL, chat.Client(), nil, ClearanceConfig{
		URL: flare.URL, Enabled: true, Timeout: 5 * time.Second,
	})
	result, err := client.RefreshAccount(context.Background(), map[string]any{"access_token": "access-token", "source_type": "chatgpt_web"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fields["email"] != "clearance@example.test" || result.Fields["quota"] != 4 {
		t.Fatalf("unexpected clearance result: %#v", result.Fields)
	}
	mu.Lock()
	defer mu.Unlock()
	if flareCalls != 1 {
		t.Fatalf("expected one FlareSolverr request, got %d", flareCalls)
	}
	if chatCalls < 3 || chatCalls > 6 {
		t.Fatalf("expected three account requests with at most three clearance retries, got %d", chatCalls)
	}
}

func TestOpenAIAccountClientUsesConfiguredProxyTransport(t *testing.T) {
	var calls int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.String() == "" || !strings.Contains(r.URL.String(), "/backend-api/") {
			t.Fatalf("proxy did not receive absolute upstream URL: %q", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "proxy@example.test", "id": "proxy-user"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 3}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()

	client := NewOpenAIAccountClient("http://upstream.invalid", "http://oauth.invalid", &http.Client{
		Transport: proxyruntime.NewTransport(http.DefaultTransport),
		Timeout:   5 * time.Second,
	}, proxyruntime.NewManager(proxy.URL, nil))
	result, err := client.RefreshAccount(context.Background(), map[string]any{
		"access_token": "access-token",
		"source_type":  "chatgpt_web",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fields["email"] != "proxy@example.test" || result.Fields["quota"] != 3 {
		t.Fatalf("unexpected proxy result: %#v", result.Fields)
	}
	if calls != 3 {
		t.Fatalf("expected three proxied endpoint calls, got %d", calls)
	}
}

func formString(r *http.Request) string {
	values, _ := url.ParseQuery(strings.TrimSpace(r.Form.Encode()))
	return values.Encode()
}
