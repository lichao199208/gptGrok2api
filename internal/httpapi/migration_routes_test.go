package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

type updateRoundTripFunc func(*http.Request) (*http.Response, error)

func (f updateRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMetaUpdateReadsProjectVersionAndChangelog(t *testing.T) {
	client := &http.Client{Transport: updateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		switch request.URL.String() {
		case projectVersionURL:
			body = "1.3.0\n"
		case projectChangelogURL:
			body = "# Changelog\n\n## 1.3.0\n\n+ [新增] 测试更新。\n"
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not found")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	server := &Server{cfg: config.Config{Version: "1.2.1-go"}, requestClient: client}
	request := httptest.NewRequest(http.MethodGet, "/meta/update", nil)
	response := httptest.NewRecorder()
	server.metaUpdate(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if stringValue(payload["status"]) != "ok" || stringValue(payload["current_version"]) != "1.2.1-go" || stringValue(payload["latest_version"]) != "1.3.0" {
		t.Fatalf("unexpected update payload: %#v", payload)
	}
	if !boolValue(payload["update_available"], false) || !strings.Contains(stringValue(payload["changelog"]), "测试更新") {
		t.Fatalf("remote update metadata was not returned: %#v", payload)
	}
	if stringValue(payload["release_url"]) != projectRepositoryURL {
		t.Fatalf("unexpected release URL: %q", stringValue(payload["release_url"]))
	}
}

func TestProjectVersionNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		newer           bool
	}{
		{latest: "1.2.2", current: "1.2.1-go", newer: true},
		{latest: "v1.2.1", current: "1.2.1-go", newer: false},
		{latest: "1.2.0", current: "1.2.1-go", newer: false},
		{latest: "2.0.0", current: "1.9.9", newer: true},
	}
	for _, test := range tests {
		if actual := projectVersionNewer(test.latest, test.current); actual != test.newer {
			t.Fatalf("projectVersionNewer(%q, %q) = %v, want %v", test.latest, test.current, actual, test.newer)
		}
	}
}

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
