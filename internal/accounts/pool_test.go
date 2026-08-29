package accounts

import (
	"context"
	"errors"
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
	p.Feedback(account, 401, errors.New("unauthorized"))
	if called != 1 {
		t.Fatalf("expected one invalid callback, got %d", called)
	}
}
