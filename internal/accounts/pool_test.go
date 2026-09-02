package accounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func TestPoolRoundRobinAndFailureCooldown(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{
		{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"},
		{"access_token": "two", "pool": "basic", "enabled": true, "status": "正常"},
	}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(first)
	pool.Feedback(first.Account, 429, nil)
	second, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release(second)
	if second.Account.Token == first.Account.Token {
		t.Fatalf("cooling account selected again: %s", second.Account.Token)
	}
}

func TestPoolFeedbackHonorsUpstreamRetryWindow(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{"email": "one@example.test"}}
	before := time.Now().Add(19 * time.Hour)
	p.Feedback(account, 429, errors.New("{\"message\":\"Please try again in 20 hours.\"}"))
	if until := p.cooldowns[account.Token]; until.Before(before) {
		t.Fatalf("expected hour-scale cooldown, got %v", until)
	}
	items, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["cooldown_until"] == nil || items[0]["cooldown_until"] == "" {
		t.Fatalf("expected persisted cooldown_until, got %#v", items)
	}
}

func TestPoolFeedbackInvokesInvalidCallbackOnlyForAuthFailures(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	called := 0
	p.SetInvalidCallback(func(account Account) {
		called++
		if account.Token != "one" {
			t.Errorf("unexpected callback account: %#v", account)
		}
	})
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{"email": "one@example.test"}}
	p.Feedback(account, 502, errors.New("upstream error"))
	if called != 0 {
		t.Fatalf("temporary upstream error invoked invalid callback %d times", called)
	}
	p.Feedback(account, 403, errors.New("cloudflare forbidden"))
	if called != 0 {
		t.Fatalf("temporary forbidden response invoked invalid callback %d times", called)
	}
	items, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0]["status"]; got != "正常" {
		t.Fatalf("403 marked account abnormal: %#v", items[0])
	}
	p.Feedback(account, 401, errors.New("unauthorized"))
	if called != 1 {
		t.Fatalf("expected one invalid callback, got %d", called)
	}
}

func TestPoolFeedbackRecordsAccountTaskCounts(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{}}
	p.Feedback(account, 200, nil)
	p.Feedback(account, 502, errors.New("upstream error"))
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}

	items, err := repository.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	if got := intValue(items[0]["success"]); got != 1 {
		t.Fatalf("success feedback was not counted: %#v", items[0])
	}
	if got := intValue(items[0]["fail"]); got != 1 {
		t.Fatalf("failure feedback was not counted: %#v", items[0])
	}
}

func TestPoolDoesNotPenalizeClientRequestErrors(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{"status": "正常"}}

	pool.Feedback(account, 400, errors.New("invalid image size"))
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	items, err := repository.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	if got := intValue(items[0]["fail"]); got != 0 {
		t.Fatalf("client request error must not count against account: %#v", items[0])
	}
	if _, err := pool.ReserveMatching(context.Background(), []string{"basic"}, nil, nil); err != nil {
		t.Fatalf("client request error must not cool down account: %v", err)
	}
}
func TestPoolSuccessfulFeedbackClearsStaleRequestAbnormalState(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	stored := map[string]any{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "异常",
		"status_reason_code": "auth_invalid", "last_error_kind": "auth_invalid",
		"last_error_status": 403, "last_error_message": "temporary forbidden", "invalid_count": 1,
	}
	if err := repository.SaveAccounts([]map[string]any{stored}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	p.Feedback(Account{Token: "one", Pool: "basic", Fields: stored}, 200, nil)
	items, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if items[0]["status"] != "正常" || intValue(items[0]["invalid_count"]) != 0 || stringValue(items[0]["last_error_kind"]) != "" {
		t.Fatalf("successful request did not recover account: %#v", items[0])
	}
}

func TestPoolConcurrencyLimitWaitsForAccountRelease(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *Lease, 1)
	errorsOut := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		lease, reserveErr := pool.ReserveMatchingLimit(ctx, []string{"basic"}, nil, nil, 1)
		if reserveErr != nil {
			errorsOut <- reserveErr
			return
		}
		acquired <- lease
	}()
	select {
	case lease := <-acquired:
		pool.Release(lease)
		t.Fatal("second request bypassed the per-account concurrency limit")
	case err := <-errorsOut:
		t.Fatalf("second request failed instead of waiting: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	pool.Release(first)
	select {
	case lease := <-acquired:
		pool.Release(lease)
	case err := <-errorsOut:
		t.Fatalf("waiting request failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("waiting request was not woken after account release")
	}
}

func TestPoolKnownImageQuotaCapsConcurrentLeases(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "正常", "quota": 1, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *Lease, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		lease, reserveErr := pool.ReserveMatchingLimit(ctx, []string{"basic"}, nil, nil, 4)
		if reserveErr == nil {
			acquired <- lease
		}
	}()
	select {
	case lease := <-acquired:
		pool.Release(lease)
		t.Fatal("second image lease bypassed known quota capacity")
	case <-time.After(40 * time.Millisecond):
	}

	pool.Release(first)
	select {
	case lease := <-acquired:
		pool.Release(lease)
	case <-time.After(time.Second):
		t.Fatal("waiting image lease was not released after the first lease ended")
	}
}

func TestPoolSkipsAccountsInTokenRefreshBackoff(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "正常",
		"next_token_refresh_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	if _, err := pool.Reserve(context.Background(), []string{"basic"}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected token-refresh backoff account to be unavailable, got %v", err)
	}

	if _, _, err := repository.UpdateAccount("one", map[string]any{
		"next_token_refresh_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatalf("account should be available after token-refresh backoff: %v", err)
	}
	pool.Release(lease)
}

func TestPoolImageQuotaIsRecordedBeforeLeaseRelease(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "正常", "quota": 1, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}

	waiting := make(chan error, 1)
	go func() {
		lease, reserveErr := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
		if reserveErr == nil {
			pool.Release(lease)
		}
		waiting <- reserveErr
	}()
	select {
	case err := <-waiting:
		t.Fatalf("second image lease must wait while quota slot is leased, got %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	pool.RecordImageConsumption(first.Account)
	pool.FeedbackImageSuccess(first.Account)
	pool.Release(first)
	select {
	case err := <-waiting:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected quota-depleted account to stay unavailable, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting reservation was not released after quota accounting")
	}
	items, err := repository.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	if got := intValue(items[0]["quota"]); got != 0 || stringValue(items[0]["status"]) != "限流" {
		t.Fatalf("expected consumed quota to make account unavailable: %#v", items[0])
	}
}

func BenchmarkPoolReserveWithTwoThousandCachedAccounts(b *testing.B) {
	root := b.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	items := make([]map[string]any, 2000)
	for index := range items {
		items[index] = map[string]any{"access_token": fmt.Sprintf("token-%04d", index), "pool": "basic", "enabled": true, "status": "正常"}
	}
	if err := repository.SaveAccounts(items); err != nil {
		b.Fatal(err)
	}
	pool := New(repository)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		lease, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
		if err != nil {
			b.Fatal(err)
		}
		pool.Release(lease)
	}
}

func TestPoolMigratesInflightSlotAcrossAccessTokenRotation(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "old-at", "refresh_token": "old-rt", "pool": "basic", "enabled": true, "status": "正常", "quota": 1, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	lease, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.RotateAccountTokens("old-at", "new-at", "new-rt", "", nil); err != nil {
		t.Fatal(err)
	}
	pool.MigrateLeaseToken(lease, "new-at")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.ReserveMatchingLimit(ctx, []string{"basic"}, nil, nil, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rotated account bypassed its in-flight image quota: %v", err)
	}

	pool.Release(lease)
	next, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
	if err != nil {
		t.Fatalf("released rotated lease did not free slot: %v", err)
	}
	defer pool.Release(next)
	if next.Account.Token != "new-at" {
		t.Fatalf("expected current token after rotation, got %q", next.Account.Token)
	}
}

func TestPoolIgnoresStaleCredentialHealthFeedbackButKeepsFailureCount(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "old-at", "refresh_token": "old-rt", "pool": "basic", "enabled": true, "status": "正常",
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	lease, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release(lease)
	invalidCalls := 0
	pool.SetInvalidCallback(func(Account) { invalidCalls++ })

	if _, _, err := repository.RotateAccountTokens("old-at", "new-at", "new-rt", "", nil); err != nil {
		t.Fatal(err)
	}
	pool.MigrateLeaseToken(lease, "new-at")
	pool.Feedback(lease.Account, 401, errors.New("old token revoked after refresh"))
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	items, err := repository.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v, %v", items, err)
	}
	account := items[0]
	if got := account["status"]; got != "正常" {
		t.Fatalf("stale 401 invalidated refreshed account: %#v", account)
	}
	if got := stringValue(account["last_error_kind"]); got != "" {
		t.Fatalf("stale 401 wrote health error markers: %#v", account)
	}
	if got := intValue(account["fail"]); got != 1 {
		t.Fatalf("stale request failure counter was lost: %#v", account)
	}
	if invalidCalls != 0 {
		t.Fatalf("stale 401 invoked invalid callback %d times", invalidCalls)
	}
}

func TestPoolQuarantinesAccountWhenImageQuotaCannotPersist(t *testing.T) {
	root := t.TempDir()
	accountsPath := filepath.Join(root, "accounts.json")
	repository := store.New(accountsPath, filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "正常", "quota": 2, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	lease, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(accountsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(accountsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.RecordImageConsumption(lease.Account); err == nil {
		t.Fatal("expected durable quota write failure")
	}
	pool.Release(lease)
	if _, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 4); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("quota-persistence failure left account schedulable: %v", err)
	}
}

func TestPoolImageQuotaDoesNotLimitUnconfiguredTextLeases(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "one", "pool": "basic", "enabled": true, "status": "正常", "quota": 1, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release(first)
	second, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 0)
	if err != nil {
		t.Fatalf("image quota unexpectedly constrained ordinary text lease: %v", err)
	}
	defer pool.Release(second)
}

func TestPoolImageReservationExcludesKnownExhaustedQuota(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{
		"access_token": "exhausted", "pool": "basic", "enabled": true, "status": "正常", "quota": 0, "image_quota_unknown": false,
	}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	if _, err := pool.ReserveMatchingImageLimit(context.Background(), []string{"basic"}, nil, nil, 4); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("known exhausted image account must be excluded from the image pool, got %v", err)
	}
	lease, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 0)
	if err != nil {
		t.Fatalf("known exhausted quota must not block ordinary text reservations: %v", err)
	}
	pool.Release(lease)
}
