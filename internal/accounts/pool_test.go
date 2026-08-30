package accounts

import (
	"context"
	"errors"
	"fmt"
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
