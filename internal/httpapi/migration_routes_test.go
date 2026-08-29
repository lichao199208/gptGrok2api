package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeImportedAccountSupportsPasswordAnd2FAAliases(t *testing.T) {
	item := normalizeImportedAccount(map[string]any{
		"accessToken":   "at-value",
		"email":         "user@example.test",
		"password":      "password-value",
		"2fa_secret":    "JBSWY3DPEHPK3PXP",
		"refresh_token": "refresh-value",
	})
	if stringValue(item["access_token"]) != "at-value" {
		t.Fatalf("access token was not normalized: %#v", item)
	}
	if stringValue(item["login_password"]) != "password-value" {
		t.Fatalf("password was not normalized: %#v", item)
	}
	if stringValue(item["two_factor_secret"]) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("2FA secret was not normalized: %#v", item)
	}
	if stringValue(item["refresh_token"]) != "refresh-value" {
		t.Fatalf("unrelated credentials were not preserved: %#v", item)
	}
}

func TestAccountImportAPIKeyFollowsSavedEnabledSetting(t *testing.T) {
	server := newAccountRefreshTestServer(t, "http://127.0.0.1.invalid")
	config, err := server.store.Config()
	if err != nil {
		t.Fatal(err)
	}
	config["account_import_api"] = map[string]any{"enabled": true, "key": "import-secret"}
	if err := server.store.ReplaceConfig(config); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/accounts/import-api", nil)
	req.Header.Set("X-API-Key", "import-secret")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("saved enabled import key was not accepted: %d %s", res.Code, res.Body.String())
	}
	config["account_import_api"] = map[string]any{"enabled": false, "key": "import-secret"}
	if err := server.store.ReplaceConfig(config); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("disabled import key was accepted: %d %s", res.Code, res.Body.String())
	}
}
