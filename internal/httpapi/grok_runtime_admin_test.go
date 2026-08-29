package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestGrokRuntimeAdminMasksTokensAndSupportsOpaqueSelectors(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	secret := "sso-secret-token-1234567890"
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]map[string]any{{
		"sso":          secret,
		"pool":         "basic",
		"status":       "active",
		"enabled":      true,
		"use_count":    3,
		"fail_count":   1,
		"last_used_at": "2030-01-01T00:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AccountsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	listed := adminRequest(handler, http.MethodGet, "/api/grok/runtime/admin/tokens", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list failed: %d %s", listed.Code, listed.Body.String())
	}
	body := listed.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("runtime token leaked in list response: %s", body)
	}
	var payload struct {
		Tokens []struct {
			Token   string `json:"token"`
			TokenID string `json:"token_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tokens) != 1 || payload.Tokens[0].Token == "" || payload.Tokens[0].TokenID == "" {
		t.Fatalf("unexpected masked token payload: %#v", payload)
	}
	if payload.Tokens[0].Token == secret || !strings.Contains(payload.Tokens[0].Token, "...") {
		t.Fatalf("token is not masked: %#v", payload.Tokens[0])
	}

	disabled := adminRequest(handler, http.MethodPost, "/api/grok/runtime/admin/tokens/disabled", strings.NewReader(`{"token_id":"`+payload.Tokens[0].TokenID+`","disabled":true}`))
	if disabled.Code != http.StatusOK || strings.Contains(disabled.Body.String(), secret) {
		t.Fatalf("opaque disable failed or leaked token: %d %s", disabled.Code, disabled.Body.String())
	}

	removed := adminRequest(handler, http.MethodDelete, "/api/grok/runtime/admin/tokens", strings.NewReader(`[`+`"`+payload.Tokens[0].TokenID+`"`+`]`))
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"deleted":1`) {
		t.Fatalf("opaque delete failed: %d %s", removed.Code, removed.Body.String())
	}
}

func TestGrokRuntimeAdminRefreshReportsNotConfigured(t *testing.T) {
	root := t.TempDir()
	handler := New(adminTestConfig(root)).Handler()

	empty := adminRequest(handler, http.MethodPost, "/api/grok/runtime/admin/batch/refresh", strings.NewReader(`{"tokens":[]}`))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty refresh should be rejected: %d %s", empty.Code, empty.Body.String())
	}

	refresh := adminRequest(handler, http.MethodPost, "/api/grok/runtime/admin/batch/refresh", strings.NewReader(`{"tokens":["tok_deadbeef"]}`))
	if refresh.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured refresh should be 503: %d %s", refresh.Code, refresh.Body.String())
	}
	if !strings.Contains(refresh.Body.String(), "not_configured") || strings.Contains(refresh.Body.String(), `"status":"completed"`) {
		t.Fatalf("refresh response must be explicit: %s", refresh.Body.String())
	}
}
