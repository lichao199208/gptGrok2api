package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterCheckoutControlsAndProbeToggle(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"checkout_tasks":[{"id":"done","status":"completed"},{"id":"waiting","status":"queued"},{"id":"retry","status":"retrying"}],"grok_probe_scheduler":{"enabled":false,"interval_minutes":15}}`
	if err := os.WriteFile(cfg.RegisterPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	stopped := adminRequest(handler, http.MethodPost, "/api/register/checkout-retries/stop", nil)
	if stopped.Code != http.StatusOK || !strings.Contains(stopped.Body.String(), `"checkout_retries_active":false`) {
		t.Fatalf("stop checkout retries failed: %d %s", stopped.Code, stopped.Body.String())
	}

	cleared := adminRequest(handler, http.MethodPost, "/api/register/checkout-history/clear", nil)
	if cleared.Code != http.StatusOK || !strings.Contains(cleared.Body.String(), `"removed":3`) {
		t.Fatalf("clear checkout history failed: %d %s", cleared.Code, cleared.Body.String())
	}

	toggled := adminRequest(handler, http.MethodPost, "/api/register/grok/probe-polling", strings.NewReader(`{"enabled":true}`))
	if toggled.Code != http.StatusOK || !strings.Contains(toggled.Body.String(), `"enabled":true`) || !strings.Contains(toggled.Body.String(), `"available":true`) {
		t.Fatalf("probe toggle response is incorrect: %d %s", toggled.Code, toggled.Body.String())
	}

	value := adminRequest(handler, http.MethodGet, "/api/register", nil)
	if value.Code != http.StatusOK || !strings.Contains(value.Body.String(), `"enabled":true`) {
		t.Fatalf("probe setting was not persisted: %d %s", value.Code, value.Body.String())
	}
}

func TestRegisterRuntimeSnapshotImportsOnlyEnabledSSOAccounts(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "sso-runtime-secret-123456"
	archive := `[{"id":"active","status":"active","enabled":true,"sso":"` + secret + `"},{"id":"disabled","status":"disabled","enabled":true,"sso":"sso-disabled"},{"id":"missing","status":"active","enabled":true}]`
	if err := os.WriteFile(cfg.GrokAccountsPath, []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()
	response := adminRequest(handler, http.MethodPost, "/api/register/grok/accounts/runtime/snapshot", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot failed: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("snapshot leaked SSO in response: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"added":1`) || !strings.Contains(response.Body.String(), `"disabled":1`) || !strings.Contains(response.Body.String(), `"missing_sso":1`) {
		t.Fatalf("unexpected snapshot summary: %s", response.Body.String())
	}
	accounts, err := New(cfg).store.AccountList()
	if err != nil || len(accounts) != 1 || stringValue(accounts[0]["access_token"]) != secret || stringValue(accounts[0]["source_type"]) != "grok_sso" {
		t.Fatalf("snapshot did not import expected Grok runtime account: %#v %v", accounts, err)
	}
}

func TestRegisterRuntimeSnapshotRefreshesRealGrokQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate-limits" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Cookie") != "sso=live-sso; sso-rw=live-sso" {
			t.Fatalf("missing SSO cookie: %q", r.Header.Get("Cookie"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"remainingQueries": 9, "totalQueries": 10, "windowSizeSeconds": 600})
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := adminTestConfig(root)
	cfg.GrokRateLimitsURL = upstream.URL + "/rate-limits"
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RegisterPath, []byte(`{"grok_probe_scheduler":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := `[{"id":"active","status":"active","enabled":true,"sso":"live-sso"}]`
	if err := os.WriteFile(cfg.GrokAccountsPath, []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(New(cfg).Handler(), http.MethodPost, "/api/register/grok/accounts/runtime/snapshot", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot failed: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"quota_refreshed":1`) || !strings.Contains(response.Body.String(), `"available":true`) {
		t.Fatalf("quota refresh was not reported: %s", response.Body.String())
	}
	runtimeRaw, err := os.ReadFile(filepath.Join(cfg.DataDir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtimeRaw), `"fast_quota"`) {
		t.Fatalf("quota was not persisted to runtime account: %s", runtimeRaw)
	}
}

func TestOpenAIAccountListHidesSynchronizedGrokAccounts(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{
		{"access_token": "legacy-openai-token", "source_type": "web"},
		{"access_token": "grok-sso-token", "source_type": "grok_sso", "type": "grok"},
	}); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(server.Handler(), http.MethodGet, "/api/accounts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("account list failed: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"total":1`) || strings.Contains(response.Body.String(), "grok-sso-token") {
		t.Fatalf("Grok account leaked into OpenAI list: %s", response.Body.String())
	}
}
