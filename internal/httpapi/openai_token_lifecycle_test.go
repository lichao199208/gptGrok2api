package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func TestEnsureOpenAIAccessTokenSharesConcurrentRefresh(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")

	var oauthCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			oauthCalls.Add(1)
			time.Sleep(100 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "refresh_token": "rotated-refresh"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			http.Error(w, "unexpected access token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "oauth@example.test", "id": "oauth-user-1"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 2}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"

	server := New(cfg)
	expiring := "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	fields := map[string]any{
		"access_token":  expiring,
		"refresh_token": "refresh-token",
		"source_type":   "oauth",
		"email":         "oauth@example.test",
		"pool":          "basic",
	}
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{fields}); err != nil {
		t.Fatal(err)
	}
	account := accounts.Account{Token: expiring, Pool: "basic", Fields: fields}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan accounts.Account, 2)
	errors := make(chan error, 2)
	for index := 0; index < 2; index++ {
		adminRefresh := index == 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var result accounts.Account
			var err error
			if adminRefresh {
				// The administrative AT endpoint uses this same helper. It must
				// share the normal request's OAuth-token rotation.
				result, err = server.refreshOpenAIAccountTokens(context.Background(), account)
			} else {
				result, err = server.ensureOpenAIAccessToken(context.Background(), account)
			}
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Fatalf("concurrent refresh failed: %v", err)
	}
	if oauthCalls.Load() != 1 {
		t.Fatalf("expected one OAuth refresh shared by request and admin refresh, got %d", oauthCalls.Load())
	}
	for result := range results {
		if result.Token != "fresh-access" {
			t.Fatalf("expected refreshed access token, got %q", result.Token)
		}
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	if items[0]["access_token"] != "fresh-access" || items[0]["refresh_token"] != "rotated-refresh" {
		t.Fatalf("refresh credentials were not saved: %#v", items[0])
	}
}

func TestReserveOpenAIAccountDoesNotCountTransientRefreshFailureAsRequestFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "temporarily unavailable"})
	}))
	defer upstream.Close()
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"

	server := New(cfg)
	expiring := "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": expiring, "refresh_token": "refresh-token", "source_type": "oauth", "email": "oauth@example.test", "pool": "basic",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := server.reserveOpenAIAccount(context.Background(), []string{"basic"}, nil, 0); err == nil {
		t.Fatal("expected transient token refresh failure")
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	item := items[0]
	if got := intValue(item["fail"]); got != 0 {
		t.Fatalf("preflight refresh failure must not increment request fail count, got %d: %#v", got, item)
	}
	if got := stringValue(item["status"]); got != "正常" {
		t.Fatalf("preflight refresh failure must not mark account abnormal, got %q: %#v", got, item)
	}
	if stringValue(item["last_token_refresh_warning"]) == "" {
		t.Fatalf("expected persisted token refresh warning: %#v", item)
	}
	until := tokenRefreshBackoffUntil(item)
	if until.Before(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("expected approximately five-minute refresh backoff, got %s", until)
	}
}

func TestAccountTokenRefreshFailureUpdatesClassifiesTerminalRefreshErrors(t *testing.T) {
	for _, message := range []string{
		"oauth refresh HTTP 400: invalid_grant",
		"oauth refresh HTTP 400: The refresh token is invalid",
		"oauth refresh HTTP 400: refresh token has expired",
		"新 access token 验证失败: openai access token is invalid",
	} {
		updates, status := accountTokenRefreshFailureUpdates(fmt.Errorf("%s", message))
		if status != http.StatusUnauthorized {
			t.Fatalf("%q classified with status %d, want %d", message, status, http.StatusUnauthorized)
		}
		if stringValue(updates["status"]) != "异常" || stringValue(updates["status_reason_code"]) != "account_invalid" {
			t.Fatalf("%q did not produce terminal account updates: %#v", message, updates)
		}
		if stringValue(updates["last_remote_check_status"]) != "token_dead" {
			t.Fatalf("%q did not persist token_dead remote status: %#v", message, updates)
		}
	}
	updates, status := accountTokenRefreshFailureUpdates(fmt.Errorf("oauth refresh HTTP 503: temporarily unavailable"))
	if status != http.StatusServiceUnavailable || stringValue(updates["next_token_refresh_at"]) == "" {
		t.Fatalf("temporary refresh failure must use retry backoff: status=%d updates=%#v", status, updates)
	}
}

func TestEnsureOpenAIAccessTokenUsesRotatedCredentialsForOldLease(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")

	var oauthCalls atomic.Int32
	fresh := "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(48*time.Hour).Unix()))) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			oauthCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fresh, "refresh_token": "fresh-refresh"})
		case "/backend-api/me":
			if r.Header.Get("Authorization") != "Bearer "+fresh {
				http.Error(w, "unexpected access token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "oauth@example.test", "id": "oauth-user-1"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"

	server := New(cfg)
	expiring := "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	fields := map[string]any{"access_token": expiring, "refresh_token": "old-refresh", "source_type": "oauth", "pool": "basic"}
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{fields}); err != nil {
		t.Fatal(err)
	}
	leaseAccount := accounts.Account{Token: expiring, Pool: "basic", Fields: fields}

	if _, err := server.refreshOpenAIAccountTokens(context.Background(), leaseAccount); err != nil {
		t.Fatal(err)
	}
	// This represents a request that had already reserved the old lease when
	// another worker completed the credential rotation. It must adopt the new
	// credentials rather than call OAuth with the old refresh token again.
	active, err := server.ensureOpenAIAccessToken(context.Background(), leaseAccount)
	if err != nil {
		t.Fatal(err)
	}
	if active.Token != fresh {
		t.Fatalf("old lease did not resolve to rotated access token: %q", active.Token)
	}
	if oauthCalls.Load() != 1 {
		t.Fatalf("expected no second OAuth exchange from stale lease, got %d calls", oauthCalls.Load())
	}
}
