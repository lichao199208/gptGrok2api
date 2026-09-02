package store

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

func TestRecordAccountRequestResultAccumulatesConcurrentCounts(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "success": 2, "fail": "3"}}); err != nil {
		t.Fatal(err)
	}

	const successes = 40
	const failures = 25
	var wait sync.WaitGroup
	for range successes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := repository.RecordAccountRequestResult("one", true, nil); err != nil {
				t.Errorf("record success: %v", err)
			}
		}()
	}
	for range failures {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := repository.RecordAccountRequestResult("one", false, map[string]any{"last_error_status": 502}); err != nil {
				t.Errorf("record failure: %v", err)
			}
		}()
	}
	wait.Wait()
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}

	items, err := repository.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	if got := nonNegativeAccountCount(items[0]["success"]); got != 2+successes {
		t.Fatalf("success count lost concurrent updates: got %d, want %d", got, 2+successes)
	}
	if got := nonNegativeAccountCount(items[0]["fail"]); got != 3+failures {
		t.Fatalf("failure count lost concurrent updates: got %d, want %d", got, 3+failures)
	}
}

func TestAccountSnapshotUsesCopyOnWriteAndFlushesRuntimeUpdates(t *testing.T) {
	root := t.TempDir()
	accountPath := filepath.Join(root, "accounts.json")
	repository := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "status": "正常", "enabled": true}}); err != nil {
		t.Fatal(err)
	}

	before, beforeRevision, err := repository.AccountSnapshot()
	if err != nil || len(before) != 1 {
		t.Fatalf("unexpected initial snapshot: %#v %v", before, err)
	}
	if _, err := repository.UpdateAccountRuntime("one", map[string]any{"last_error_status": 502, "last_error_message": "temporary"}); err != nil {
		t.Fatal(err)
	}
	after, afterRevision, err := repository.AccountSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision <= beforeRevision || after[0]["last_error_message"] != "temporary" {
		t.Fatalf("runtime update did not advance snapshot: before=%d after=%d item=%#v", beforeRevision, afterRevision, after[0])
	}
	if before[0]["last_error_message"] != nil {
		t.Fatalf("previous snapshot was mutated in place: %#v", before[0])
	}
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0]["last_error_message"] != "temporary" {
		t.Fatalf("runtime update was not flushed: %#v", persisted)
	}
}

func TestRotatedAccessTokenAliasPreservesInFlightImageAccounting(t *testing.T) {
	root := t.TempDir()
	accountPath := filepath.Join(root, "accounts.json")
	repository := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if _, _, _, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token":        "old-access",
		"refresh_token":       "old-refresh",
		"quota":               1,
		"image_quota_unknown": false,
		"status":              "正常",
		"source_type":         "oauth",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.RotateAccountTokens("old-access", "new-access", "new-refresh", "", nil); err != nil {
		t.Fatal(err)
	}

	// Simulate a worker that reserved the old access token before the OAuth
	// rotation and finishes image generation afterwards.
	if _, err := repository.RecordAccountImageConsumption("old-access", nil); err != nil {
		t.Fatalf("old in-flight token must resolve to rotated account: %v", err)
	}
	if _, err := repository.RecordAccountImageResult("old-access", true, nil); err != nil {
		t.Fatalf("old in-flight token must retain its final result: %v", err)
	}

	// Quota consumption is deliberately synchronous. It must survive a new
	// Store instance without relying on the normal delayed runtime flush.
	reloaded := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	items, err := reloaded.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected reloaded accounts: %#v, %v", items, err)
	}
	if got := accountToken(items[0]); got != "new-access" {
		t.Fatalf("expected rotated token, got %q", got)
	}
	if got := nonNegativeAccountCount(items[0]["quota"]); got != 0 {
		t.Fatalf("quota was not synchronously persisted: got %d", got)
	}
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	items, err = repository.AccountList()
	if err != nil || nonNegativeAccountCount(items[0]["success"]) != 1 {
		t.Fatalf("in-flight success was not attached to current account: %#v, %v", items, err)
	}
}

func TestCredentialGenerationRejectsStaleRefreshWrite(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if _, _, _, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token": "old-access", "refresh_token": "old-refresh", "status": "正常", "source_type": "oauth",
	}}); err != nil {
		t.Fatal(err)
	}
	_, _, generation, err := repository.CredentialSnapshot("old-access")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.RotateAccountTokens("old-access", "new-access", "new-refresh", "", map[string]any{"status": "正常"}); err != nil {
		t.Fatal(err)
	}

	updated, applied, err := repository.UpdateAccountIfCredentials("old-access", generation, map[string]any{"status": "异常", "last_token_refresh_error": "stale error"})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatalf("stale refresh outcome unexpectedly overwrote current credentials: %#v", updated)
	}
	if accountToken(updated) != "new-access" || updated["status"] != "正常" || updated["last_token_refresh_error"] != nil {
		t.Fatalf("expected current rotated account to be retained, got %#v", updated)
	}
}
