package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		RootDir:      ".",
		DataDir:      "data",
		StaticDir:    "web_dist",
		ConfigPath:   "config.json",
		AuthKeysPath: "data/auth_keys.json",
		APIKey:       "api-secret",
		AdminKey:     "admin-secret",
		Version:      "test",
	}
}

func TestHealthDoesNotRequireAuthentication(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()

	New(testConfig()).Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"runtime":"go"`) {
		t.Fatalf("unexpected health response: %s", response.Body.String())
	}
}

func TestModelsRequireAuthentication(t *testing.T) {
	handler := New(testConfig()).Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer api-secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", authorized.Code)
	}
}

func TestAuthProtocolUsesBearerHeaderAndSoftStatusProbe(t *testing.T) {
	handler := New(testConfig()).Handler()

	loginRequest := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	loginRequest.Header.Set("Authorization", "Bearer admin-secret")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK || !strings.Contains(loginResponse.Body.String(), `"role":"admin"`) {
		t.Fatalf("unexpected login response: %d %s", loginResponse.Code, loginResponse.Body.String())
	}

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/auth/status", nil))
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"authenticated":false`) {
		t.Fatalf("unexpected unauthenticated status response: %d %s", statusResponse.Code, statusResponse.Body.String())
	}
}

func TestAccountListDoesNotExposeCredentialTokens(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.AccountsPath = root + "\\accounts.json"
	cfg.AuthKeysPath = root + "\\auth_keys.json"
	cfg.ConfigPath = root + "\\config.json"
	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token":  "access-secret",
		"refresh_token": "refresh-secret",
		"id_token":      "id-secret",
		"email":         "account@example.test",
	}}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("account list returned %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{"access-secret", "refresh-secret", "id-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("account list leaked credential %q: %s", secret, body)
		}
	}
}

func TestAccountStatusCategoryIgnoresClearedMarkers(t *testing.T) {
	account := map[string]any{
		"status":             "正常",
		"quota":              23,
		"last_refresh_error": nil,
		"last_error_kind":    nil,
		"status_reason_code": nil,
	}
	if category := accountStatusCategory(account); category != "normal" {
		t.Fatalf("expected healthy account, got %q", category)
	}
}
