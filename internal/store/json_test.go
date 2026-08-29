package store

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthKeyLifecycleKeepsPrivateHashOutOfPublicPayload(t *testing.T) {
	root := t.TempDir()
	repository := New(
		filepath.Join(root, "accounts.json"),
		filepath.Join(root, "auth_keys.json"),
		filepath.Join(root, "config.json"),
	)

	item, rawKey, err := repository.CreateKey("user", "", "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if rawKey == "" || item["key_hash"] != nil {
		t.Fatalf("public key payload leaked private material: %#v", item)
	}

	identity, ok := repository.Authenticate(rawKey)
	if !ok || identity.Role != "user" {
		t.Fatalf("created key did not authenticate: %#v", identity)
	}
}

func TestAccountStoragePreservesJSONCompatibleFields(t *testing.T) {
	root := t.TempDir()
	accountPath := filepath.Join(root, "accounts.json")
	repository := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))

	added, skipped, items, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token":  "token-1",
		"refresh_token": "refresh-secret",
		"email":         "user@example.test",
	}})
	if err != nil || added != 1 || skipped != 0 {
		t.Fatalf("unexpected add result: added=%d skipped=%d err=%v", added, skipped, err)
	}
	if len(items) != 1 || items[0]["access_token"] != "token-1" {
		t.Fatalf("unexpected stored accounts: %#v", items)
	}

	raw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("stored account JSON is invalid: %v", err)
	}
	if decoded[0]["refresh_token"] != "refresh-secret" {
		t.Fatalf("account secret was not preserved in storage")
	}
}

func TestValidatorUsesStoredUserKeys(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	_, rawKey, err := repository.CreateKey("user", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+rawKey)
	identity, ok := repository.Authenticate(rawKey)
	if !ok || identity.Role != "user" {
		t.Fatalf("stored user key was not accepted")
	}
}

func TestRotateAccountTokensClearsStaleErrorMarkers(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	_, _, _, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token":             "old-token",
		"status":                   "异常",
		"status_reason_code":       "auth_invalid",
		"last_refresh_error":       "old refresh failure",
		"last_refresh_error_at":    "2026-08-23T10:00:00Z",
		"last_error_kind":          "auth_invalid",
		"last_error_status":        401,
		"last_error_message":       "expired",
		"last_error_at":            "2026-08-23T10:00:00Z",
		"last_token_refresh_error": "old token failure",
	}})
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := repository.RotateAccountTokens(
		"old-token",
		"new-token",
		"new-refresh",
		"",
		map[string]any{"quota": 23, "status": "正常"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated["status"] != "正常" || updated["quota"] != 23 {
		t.Fatalf("remote fields were not persisted: %#v", updated)
	}
	for _, key := range []string{
		"last_refresh_error",
		"last_refresh_error_at",
		"last_error_kind",
		"last_error_status",
		"last_error_message",
		"last_error_at",
		"last_token_refresh_error",
		"last_token_refresh_error_at",
		"status_reason_code",
	} {
		if updated[key] != nil {
			t.Fatalf("stale marker %q was not cleared: %#v", key, updated[key])
		}
	}
}

func TestDeletedAccountTombstoneNormalizesSSOPrefix(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if added, _, _, err := repository.AddAccounts(nil, []map[string]any{{"sso": "sso=opaque-token", "source_type": "grok_sso"}}); err != nil || added != 1 {
		t.Fatalf("initial SSO add failed: added=%d err=%v", added, err)
	}
	if removed, _, err := repository.DeleteAccounts([]string{"opaque-token"}); err != nil || removed != 1 {
		t.Fatalf("SSO delete failed: removed=%d err=%v", removed, err)
	}
	added, skipped, _, err := repository.AddAccounts(nil, []map[string]any{{"sso": "sso=opaque-token", "source_type": "grok_sso"}})
	if err != nil || added != 0 || skipped != 1 {
		t.Fatalf("SSO tombstone was bypassed: added=%d skipped=%d err=%v", added, skipped, err)
	}
}

func TestDeletedAccountCannotBeReaddedByAutomaticImport(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if added, _, _, err := repository.AddAccounts([]string{"deleted-token"}, nil); err != nil || added != 1 {
		t.Fatalf("initial add failed: added=%d err=%v", added, err)
	}
	removed, _, err := repository.DeleteAccounts([]string{"deleted-token"})
	if err != nil || removed != 1 {
		t.Fatalf("delete failed: removed=%d err=%v", removed, err)
	}
	added, skipped, items, err := repository.AddAccounts([]string{"deleted-token"}, []map[string]any{{"access_token": "deleted-token", "source_type": "grok_sso"}})
	if err != nil || added != 0 || skipped != 2 || len(items) != 0 {
		t.Fatalf("deleted token was re-added: added=%d skipped=%d items=%#v err=%v", added, skipped, items, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "deleted_accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || string(raw) == "[\"deleted-token\"]" {
		t.Fatalf("deletion tombstone is missing or contains plaintext token: %s", raw)
	}
}
