package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"github.com/auucoder/gptgrok2api-go/internal/store"
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

type trackedRequestBody struct {
	reads int
}

func (b *trackedRequestBody) Read(_ []byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func (b *trackedRequestBody) Close() error { return nil }

func TestMonitoredRequestAuthenticatesBeforeReadingBody(t *testing.T) {
	body := &trackedRequestBody{}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Content-Type", "application/json")
	request.Body = body
	request.ContentLength = -1
	response := httptest.NewRecorder()

	New(testConfig()).Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", response.Code, response.Body.String())
	}
	if body.reads != 0 {
		t.Fatalf("unauthenticated monitored request body was read %d times", body.reads)
	}
}

func TestDecodeJSONAcceptsWrappedJSONObject(t *testing.T) {
	wrapped, err := json.Marshal(`{"name":"demo"}`)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(wrapped)))
	response := httptest.NewRecorder()
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(response, request, &body) {
		t.Fatalf("wrapped object was rejected: %s", response.Body.String())
	}
	if body.Name != "demo" {
		t.Fatalf("wrapped object decoded incorrectly: %#v", body)
	}
}

func TestMonitorRequestShapeAcceptsWrappedJSONObject(t *testing.T) {
	wrapped, err := json.Marshal(`{"model":"gpt-test","prompt":"describe this"}`)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(wrapped)))
	request.Header.Set("Content-Type", "application/json")
	model, summary, shape := monitorRequestShape(request)
	if model != "gpt-test" || summary != "describe this" {
		t.Fatalf("wrapped request was not monitored correctly: model=%q summary=%q", model, summary)
	}
	shapeMap, ok := shape.(map[string]any)
	if !ok || shapeMap["content_type"] != "application/json" {
		t.Fatalf("unexpected request shape: %#v", shape)
	}
}

func TestMonitorRequestShapeIncludesRequestedImageCount(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"make two","n":2}`))
	request.Header.Set("Content-Type", "application/json")
	_, _, shape := monitorRequestShape(request)
	shapeMap, ok := shape.(map[string]any)
	if !ok || intValue(shapeMap["requested_n"]) != 2 {
		t.Fatalf("requested image count was not recorded: %#v", shape)
	}
}

func TestUnwrapDoubleEncodedJSONObjectRejectsOversizedPayload(t *testing.T) {
	wrapped, err := json.Marshal("{" + strings.Repeat("x", maxWrappedJSONBodyBytes) + "}")
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) <= maxWrappedJSONBodyBytes {
		t.Fatalf("test payload did not exceed wrapped limit: %d", len(wrapped))
	}
	actual, ok := unwrapDoubleEncodedJSONObject(wrapped)
	if ok || string(actual) != string(wrapped) {
		t.Fatal("oversized wrapped payload was accepted")
	}
}

func TestImageConcurrencySlotWaitsForRelease(t *testing.T) {
	server := &Server{imageSlots: makeImageSlots(1)}
	firstRelease, err := server.acquireImageSlot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	errorsOut := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		release, acquireErr := server.acquireImageSlot(ctx, nil)
		if acquireErr != nil {
			errorsOut <- acquireErr
			return
		}
		acquired <- release
	}()
	select {
	case release := <-acquired:
		release()
		t.Fatal("second task bypassed the global image limit")
	case err := <-errorsOut:
		t.Fatalf("second task failed instead of waiting: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	firstRelease()
	select {
	case release := <-acquired:
		release()
	case err := <-errorsOut:
		t.Fatalf("waiting task failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("waiting task was not woken after slot release")
	}
}

func TestRefreshProxyRuntimeAppliesPersistedFallbackGroup(t *testing.T) {
	for _, key := range []string{"GO_PROXY_URL", "GO_PROXY_POOL", "GO_RESOURCE_PROXY_URL", "GO_RESOURCE_PROXY_POOL"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.ReplaceConfig(map[string]any{
		"proxy":          "http://old-default.invalid:8080",
		"fallback_proxy": "group:images",
		"proxy_groups": []any{map[string]any{
			"id": "images", "enabled": true,
			"nodes": []any{map[string]any{
				"id": "node-one", "url": "socks5://group.invalid:1080", "enabled": true,
				"last_status": 403, "image_concurrency_limit": 1,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: repository, proxyManager: proxyruntime.NewManager("http://stale.invalid:8080", nil)}
	if err := server.refreshProxyRuntime(); err != nil {
		t.Fatal(err)
	}
	lease, err := server.proxyManager.AcquireImageContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(false)
	if lease.Source != "group" || lease.GroupID != "images" || lease.NodeID != "node-one" {
		t.Fatalf("persisted proxy group was not applied: %#v", lease)
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

func TestAccountAutoRemoveInvalidRequiresDefinitiveExpiredToken(t *testing.T) {
	tests := []struct {
		name string
		item map[string]any
		want bool
	}{
		{
			name: "expired browser token without refresh token",
			item: map[string]any{"access_token": "expired", "status": "异常", "status_reason_code": "auth_invalid", "last_error_status": 401},
			want: true,
		},
		{
			name: "oauth token retained for refresh",
			item: map[string]any{"access_token": "expired", "refresh_token": "refresh", "status": "异常", "status_reason_code": "auth_invalid", "last_error_status": 401},
			want: false,
		},
		{
			name: "temporary upstream error retained",
			item: map[string]any{"access_token": "temporary", "status": "正常", "last_error_kind": "upstream_error", "last_error_status": 503},
			want: false,
		},
		{
			name: "forbidden proxy response retained",
			item: map[string]any{"access_token": "forbidden", "status": "正常", "last_error_kind": "upstream_error", "last_error_status": 403},
			want: false,
		},
		{
			name: "password and two factor account retained",
			item: map[string]any{"access_token": "recoverable", "status": "异常", "status_reason_code": "auth_invalid", "last_error_status": 401, "login_password": "secret", "two_factor_secret": "JBSWY3DPEHPK3PXP"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := accountAutoRemoveInvalid(test.item); got != test.want {
				t.Fatalf("accountAutoRemoveInvalid() = %v, want %v: %#v", got, test.want, test.item)
			}
		})
	}
}

func TestAccountStatusCategoryTreatsKnownExhaustedQuotaAsLimited(t *testing.T) {
	tests := []struct {
		name string
		item map[string]any
		want string
	}{
		{
			name: "zero quota",
			item: map[string]any{"status": "正常", "quota": 0, "image_quota_unknown": false},
			want: "limited",
		},
		{
			name: "negative quota",
			item: map[string]any{"status": "正常", "quota": -21, "image_quota_unknown": false},
			want: "limited",
		},
		{
			name: "unknown quota remains normal",
			item: map[string]any{"status": "正常", "quota": 0, "image_quota_unknown": true},
			want: "normal",
		},
		{
			name: "missing quota remains normal",
			item: map[string]any{"status": "正常", "image_quota_unknown": false},
			want: "normal",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := accountStatusCategory(test.item); got != test.want {
				t.Fatalf("accountStatusCategory() = %q, want %q: %#v", got, test.want, test.item)
			}
		})
	}
}

func TestAccountStatusCategoryPrioritizesConfirmedCredentialFailureOverLimit(t *testing.T) {
	account := map[string]any{
		"status":                   "限流",
		"quota":                    0,
		"image_quota_unknown":      false,
		"last_remote_check_status": "token_dead",
		"survival_status":          "token_dead",
		"last_remote_check_error":  "openai access token is invalid",
		"survival_check_error":     "openai access token is invalid",
	}
	if category := accountStatusCategory(account); category != "abnormal" {
		t.Fatalf("confirmed invalid credentials must outrank exhausted quota, got %q", category)
	}
}

func TestAccountStatusCategoryRecognizesOpenAIInvalidTokenErrorAsAbnormal(t *testing.T) {
	account := map[string]any{
		"status":              "限流",
		"quota":               0,
		"image_quota_unknown": false,
		"last_refresh_error":  "openai access token is invalid",
		"last_error_kind":     "auth_invalid",
		"status_reason_code":  "account_invalid",
		"last_error_status":   401,
	}
	if category := accountStatusCategory(account); category != "abnormal" {
		t.Fatalf("invalid access token must outrank exhausted quota, got %q", category)
	}
}

func TestAccountStatusCategoryKeepsPersistedAbnormalStatusAbnormal(t *testing.T) {
	account := map[string]any{
		"status":         "异常",
		"quota":          12,
		"survival_alive": true,
	}
	if category := accountStatusCategory(account); category != "abnormal" {
		t.Fatalf("persisted abnormal status must remain abnormal, got %q", category)
	}
}

func TestAccountStatusCategoryDoesNotTreatTransientMarkersAsAbnormal(t *testing.T) {
	account := map[string]any{
		"status": "正常", "quota": 7, "last_error_kind": "upstream_error",
		"last_error_status": 403, "last_refresh_error": "temporary proxy failure", "invalid_count": 1,
	}
	if category := accountStatusCategory(account); category != "normal" {
		t.Fatalf("expected transient markers to remain normal, got %q", category)
	}
}
