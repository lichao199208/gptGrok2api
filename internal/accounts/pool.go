package accounts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

var ErrUnavailable = errors.New("no available accounts")

var retryAfterPattern = regexp.MustCompile(`(?i)(\d+)\s*(hours?|hrs?|小时|minutes?|mins?|分钟)`)

type Account struct {
	Token  string
	Pool   string
	Fields map[string]any
}

type Lease struct {
	Account Account
	pool    *Pool
	once    sync.Once
}

type Pool struct {
	repository *store.Store
	onInvalid  func(Account)
	mu         sync.Mutex
	next       uint64
	inflight   map[string]int
	cooldowns  map[string]time.Time
	failures   map[string]int
	wake       chan struct{}
	accounts   []Account
	revision   uint64
}

// SetInvalidCallback registers a callback for definitive credential failures.
// The callback is invoked after account state has been persisted and is kept
// outside the pool package so the HTTP layer can apply its configured policy.
func (p *Pool) SetInvalidCallback(callback func(Account)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onInvalid = callback
}

func New(repository *store.Store) *Pool {
	return &Pool{
		repository: repository,
		inflight:   map[string]int{},
		cooldowns:  map[string]time.Time{},
		failures:   map[string]int{},
		wake:       make(chan struct{}),
	}
}

func (p *Pool) Reserve(ctx context.Context, pools []string, excluded map[string]bool) (*Lease, error) {
	return p.ReserveMatching(ctx, pools, excluded, nil)
}

// ReserveMatching is the common account leasing path with an optional
// provider predicate. This keeps OpenAI JWT accounts separate from Grok SSO
// accounts even when both are stored in the same account file.
func (p *Pool) ReserveMatching(ctx context.Context, pools []string, excluded map[string]bool, match func(Account) bool) (*Lease, error) {
	return p.ReserveMatchingLimit(ctx, pools, excluded, match, 0)
}

// ReserveMatchingLimit waits when every matching account is at its concurrency
// limit. A non-positive limit preserves the legacy least-busy behavior.
func (p *Pool) ReserveMatchingLimit(ctx context.Context, pools []string, excluded map[string]bool, match func(Account) bool, maxInflight int) (*Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items, revision, err := p.repository.AccountSnapshot()
		if err != nil {
			return nil, fmt.Errorf("load accounts: %w", err)
		}

		now := time.Now()
		p.mu.Lock()
		if p.revision != revision {
			p.accounts = normalizeAccounts(items)
			p.revision = revision
		}
		capacityBlocked := false
		minInflight := int(^uint(0) >> 1)
		var selected Account
		found := false
		start := 0
		if len(p.accounts) > 0 {
			start = int(p.next % uint64(len(p.accounts)))
		}
		for offset := 0; offset < len(p.accounts); offset++ {
			account := p.accounts[(start+offset)%len(p.accounts)]
			if excluded[account.Token] || (match != nil && !match(account)) || !p.available(account, pools, now) {
				continue
			}
			inflight := p.inflight[account.Token]
			if maxInflight > 0 && inflight >= maxInflight {
				capacityBlocked = true
				continue
			}
			if inflight < minInflight {
				minInflight = inflight
				selected = account
				found = true
			}
		}
		if !found {
			wake := p.wake
			p.mu.Unlock()
			if !capacityBlocked {
				return nil, ErrUnavailable
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wake:
				continue
			}
		}

		// Starting at a rotating offset preserves tie fairness without building
		// a temporary candidate list for every request.
		p.next++
		p.inflight[selected.Token]++
		p.mu.Unlock()
		return &Lease{Account: selected, pool: p}, nil
	}
}

func (p *Pool) Release(lease *Lease) {
	if lease == nil || lease.pool != p {
		return
	}
	lease.once.Do(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.inflight[lease.Account.Token] <= 1 {
			delete(p.inflight, lease.Account.Token)
		} else {
			p.inflight[lease.Account.Token]--
		}
		p.signalLocked()
	})
}

func (p *Pool) signalLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

func (p *Pool) Feedback(account Account, status int, err error) {
	if account.Token == "" {
		return
	}
	p.mu.Lock()
	if status >= 200 && status < 300 {
		delete(p.cooldowns, account.Token)
		delete(p.failures, account.Token)
		p.mu.Unlock()
		// A completed upstream request is positive proof that the credential is
		// usable. Clear stale request markers left by an earlier proxy/CDN error.
		if hasRequestErrorMarkers(account.Fields) {
			updates := map[string]any{
				"last_error_kind":    nil,
				"last_error_status":  nil,
				"last_error_message": nil,
				"last_error_at":      nil,
				"status_reason_code": nil,
				"invalid_count":      0,
				"cooldown_until":     nil,
			}
			if isRequestMarkedAbnormal(account.Fields) {
				updates["status"] = "正常"
			}
			_, _ = p.repository.RecordAccountRequestResult(account.Token, true, updates)
		} else {
			_, _ = p.repository.RecordAccountRequestResult(account.Token, true, nil)
		}
		return
	}
	p.failures[account.Token]++
	failureCount := p.failures[account.Token]
	cooldown := time.Duration(5*(1<<min(failureCount-1, 5))) * time.Second
	if status == 401 {
		cooldown = 10 * time.Minute
	} else if status == 429 {
		if parsed := retryAfterDuration(err); parsed > 0 {
			cooldown = parsed
		}
	}
	p.cooldowns[account.Token] = time.Now().Add(cooldown)
	p.mu.Unlock()

	updates := map[string]any{
		"last_error_kind":   "upstream_error",
		"last_error_status": status,
		"last_error_at":     time.Now().UTC().Format(time.RFC3339),
	}
	if status == 401 {
		updates["status"] = "异常"
		updates["status_reason_code"] = "auth_invalid"
		updates["last_error_kind"] = "auth_invalid"
	} else if status == 429 {
		updates["status_reason_code"] = "lane_backoff"
		updates["last_error_kind"] = "rate_limited"
		updates["cooldown_until"] = time.Now().Add(cooldown).UTC().Format(time.RFC3339)
	}
	if err != nil {
		updates["last_error_message"] = truncate(err.Error(), 300)
	}
	_, _ = p.repository.RecordAccountRequestResult(account.Token, false, updates)
	if status == 401 {
		p.mu.Lock()
		callback := p.onInvalid
		p.mu.Unlock()
		if callback != nil {
			callback(account)
		}
	}
}

func hasRequestErrorMarkers(fields map[string]any) bool {
	if fields == nil {
		return false
	}
	for _, key := range []string{"last_error_kind", "last_error_status", "last_error_message", "last_error_at", "status_reason_code", "cooldown_until"} {
		if fields[key] != nil && strings.TrimSpace(fmt.Sprint(fields[key])) != "" {
			return true
		}
	}
	return intValue(fields["invalid_count"]) > 0
}

func isRequestMarkedAbnormal(fields map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(stringValue(fields["status"])))
	reason := strings.ToLower(strings.TrimSpace(stringValue(fields["status_reason_code"])))
	kind := strings.ToLower(strings.TrimSpace(stringValue(fields["last_error_kind"])))
	return status == "异常" || status == "abnormal" || status == "invalid" || status == "unauthorized" ||
		reason == "auth_invalid" || reason == "account_invalid" || kind == "auth_invalid"
}

func retryAfterDuration(err error) time.Duration {
	if err == nil {
		return 0
	}
	matches := retryAfterPattern.FindStringSubmatch(err.Error())
	if len(matches) != 3 {
		return 0
	}
	amount, parseErr := strconv.Atoi(matches[1])
	if parseErr != nil || amount <= 0 {
		return 0
	}
	unit := strings.ToLower(matches[2])
	if strings.Contains(unit, "hour") || strings.Contains(unit, "hr") || strings.Contains(unit, "小时") {
		return time.Duration(amount) * time.Hour
	}
	return time.Duration(amount) * time.Minute
}

func (p *Pool) available(account Account, pools []string, now time.Time) bool {
	if !poolAllowed(account.Pool, pools) {
		return false
	}
	if until := p.cooldowns[account.Token]; until.After(now) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(stringValue(account.Fields["status"])))
	if !boolValue(account.Fields["enabled"], true) {
		return false
	}
	switch status {
	case "disabled", "auto_disabled", "禁用", "abnormal", "invalid", "error", "incomplete", "异常", "expired", "unauthorized":
		if looksRecovered(account.Fields) {
			break
		}
		return false
	case "limited", "rate_limited", "cooling", "backoff", "限流":
		return false
	}
	if until := timestamp(account.Fields, "cooldown_until", "cooldown_until_ms", "next_retry_at"); until.After(now) {
		return false
	}
	return true
}

func looksRecovered(fields map[string]any) bool {
	if intValue(fields["quota"]) <= 0 && !boolValue(fields["survival_alive"], false) {
		return false
	}
	if intValue(fields["invalid_count"]) > 0 {
		return false
	}
	for _, key := range []string{"last_refresh_error", "last_token_refresh_error", "last_error_message"} {
		if stringValue(fields[key]) != "" {
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(fields["last_error_kind"]))) {
	case "auth_invalid", "parse_failure", "upstream_error":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(fields["status_reason_code"]))) {
	case "account_invalid", "snlm0e_refresh_failed", "parse_failure", "upstream_error":
		return false
	}
	return true
}

func normalize(item map[string]any) (Account, bool) {
	token := firstString(item, "access_token", "accessToken", "token", "sso", "session_token")
	token = strings.TrimSpace(strings.TrimPrefix(token, "sso="))
	if token == "" {
		return Account{}, false
	}
	pool := strings.ToLower(firstString(item, "pool", "account_type", "type", "tier"))
	switch pool {
	case "super", "ssosuper":
		pool = "super"
	case "heavy", "ssoheavy":
		pool = "heavy"
	default:
		pool = "basic"
	}
	return Account{Token: token, Pool: pool, Fields: item}, true
}

func normalizeAccounts(items []map[string]any) []Account {
	accounts := make([]Account, 0, len(items))
	for _, item := range items {
		if account, ok := normalize(item); ok {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func poolAllowed(accountPool string, pools []string) bool {
	for _, pool := range pools {
		if strings.EqualFold(pool, accountPool) {
			return true
		}
	}
	return false
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(item[key]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return strings.TrimSpace(typed)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		var parsed int
		_, _ = fmt.Sscanf(fmt.Sprint(value), "%d", &parsed)
		return parsed
	}
}

func boolValue(value any, fallback bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "0", "false", "no", "off":
			return false
		case "1", "true", "yes", "on":
			return true
		}
	}
	return fallback
}

func timestamp(item map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		value := item[key]
		switch typed := value.(type) {
		case float64:
			if typed > 1e12 {
				return time.UnixMilli(int64(typed))
			}
			if typed > 0 {
				return time.Unix(int64(typed), 0)
			}
		case int64:
			return time.Unix(typed, 0)
		case string:
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(typed)); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
