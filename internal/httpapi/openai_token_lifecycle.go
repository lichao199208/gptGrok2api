package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

const accessTokenRefreshBackoff = 5 * time.Minute

type accessTokenRefreshCall struct {
	done    chan struct{}
	account accounts.Account
	err     error
}

// reserveOpenAIAccount is the only normal-request leasing path for OAuth GPT
// accounts. It refreshes an expiring JWT before that token is sent upstream.
func (s *Server) reserveOpenAIAccount(ctx context.Context, pools []string, excluded map[string]bool, maxInflight int) (*accounts.Lease, error) {
	return s.reserveOpenAIAccountWithMode(ctx, pools, excluded, maxInflight, false)
}

func (s *Server) reserveOpenAIImageAccount(ctx context.Context, pools []string, excluded map[string]bool, maxInflight int) (*accounts.Lease, error) {
	return s.reserveOpenAIAccountWithMode(ctx, pools, excluded, maxInflight, true)
}

func (s *Server) reserveOpenAIAccountWithMode(ctx context.Context, pools []string, excluded map[string]bool, maxInflight int, imageOnly bool) (*accounts.Lease, error) {
	var (
		lease *accounts.Lease
		err   error
	)
	if imageOnly {
		lease, err = s.accountPool.ReserveMatchingImageLimit(ctx, pools, excluded, isOpenAIAccount, maxInflight)
	} else {
		lease, err = s.accountPool.ReserveMatchingLimit(ctx, pools, excluded, isOpenAIAccount, maxInflight)
	}
	if err != nil {
		return nil, err
	}
	active, err := s.ensureOpenAIAccessToken(ctx, lease.Account)
	if err != nil {
		s.accountPool.Release(lease)
		// Token refresh is a preflight operation, not an upstream execution of
		// the caller's request. refreshOpenAIAccessToken already persists either
		// a terminal auth state or a short retry warning, so recording ordinary
		// request feedback here would double-count failures and add the normal
		// request cooldown to a transient OAuth outage.
		return nil, err
	}
	// Token rotation changes the token used by provider requests. Move pool
	// runtime state first so new reservations see this lease's existing inflight
	// slot and cannot oversubscribe a known image quota.
	s.accountPool.MigrateLeaseToken(lease, active.Token)
	lease.Account = active
	return lease, nil
}

var errOpenAICredentialsChanged = errors.New("OpenAI account credentials changed during refresh")

func (s *Server) ensureOpenAIAccessToken(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	return s.maintainOpenAIAccessToken(ctx, account, false)
}

// refreshOpenAIAccountTokens is the explicit/admin refresh path. It preserves
// the endpoint's existing force-refresh behavior, while normal requests and
// survival checks use ensureOpenAIAccessToken and respect retry backoff.
func (s *Server) refreshOpenAIAccountTokens(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	return s.maintainOpenAIAccessToken(ctx, account, true)
}

// maintainOpenAIAccessToken follows the reference service's narrow refresh
// protocol: resolve an old lease to the current token, snapshot its credential
// generation, then single-flight only that exact generation. If a concurrent
// refresh rotates credentials, a stale caller re-reads the current account
// instead of reusing its former refresh token.
func (s *Server) maintainOpenAIAccessToken(ctx context.Context, account accounts.Account, force bool) (accounts.Account, error) {
	for attempt := 0; attempt < 2; attempt++ {
		_, fields, generation, err := s.store.CredentialSnapshot(account.Token)
		if err != nil {
			return account, err
		}
		active := openAIAccountFromFields(fields)
		if active.Pool == "" {
			active.Pool = account.Pool
		}
		if !hasOpenAIRefreshToken(active) || (!force && !provider.AccessTokenNeedsRefresh(active.Token)) {
			return active, nil
		}
		if !force {
			if until := tokenRefreshBackoffUntil(active.Fields); until.After(time.Now()) {
				return active, temporaryTokenRefreshError(fmt.Sprintf("access token refresh is temporarily backed off until %s", until.UTC().Format(time.RFC3339)))
			}
		}

		refreshed, refreshErr := s.refreshOpenAICredentialGeneration(ctx, active, generation)
		if errors.Is(refreshErr, errOpenAICredentialsChanged) {
			account = active
			continue
		}
		return refreshed, refreshErr
	}
	return account, errOpenAICredentialsChanged
}

func (s *Server) refreshOpenAICredentialGeneration(ctx context.Context, account accounts.Account, generation store.CredentialGeneration) (accounts.Account, error) {
	key := tokenRefreshKey(generation)
	s.tokenRefreshMu.Lock()
	if existing := s.tokenRefreshes[key]; existing != nil {
		s.tokenRefreshMu.Unlock()
		select {
		case <-existing.done:
			return existing.account, existing.err
		case <-ctx.Done():
			return account, ctx.Err()
		}
	}
	call := &accessTokenRefreshCall{done: make(chan struct{}), account: account}
	s.tokenRefreshes[key] = call
	s.tokenRefreshMu.Unlock()

	call.account, call.err = s.refreshOpenAIAccessToken(ctx, account, generation)
	close(call.done)

	s.tokenRefreshMu.Lock()
	delete(s.tokenRefreshes, key)
	s.tokenRefreshMu.Unlock()
	return call.account, call.err
}

func openAIAccountFromFields(account map[string]any) accounts.Account {
	token := accountToken(account)
	return accounts.Account{
		Token:                token,
		Pool:                 strings.TrimSpace(stringValue(account["pool"])),
		Fields:               account,
		CredentialGeneration: store.CredentialGenerationForAccount(token, account),
	}
}

func hasOpenAIRefreshToken(account accounts.Account) bool {
	if account.Token == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(stringValue(account.Fields["source_type"])), "chatgpt_web") {
		return false
	}
	return strings.TrimSpace(stringValue(account.Fields["refresh_token"])) != ""
}

func canRefreshOpenAIAccessToken(account accounts.Account) bool {
	return hasOpenAIRefreshToken(account) && provider.AccessTokenNeedsRefresh(account.Token)
}

func tokenRefreshKey(generation store.CredentialGeneration) string {
	return generation.AccessToken + "\x00" + generation.RefreshToken + "\x00" + generation.LastTokenRefreshAt
}

func tokenRefreshBackoffUntil(fields map[string]any) time.Time {
	value := strings.TrimSpace(stringValue(fields["next_token_refresh_at"]))
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (s *Server) refreshOpenAIAccessToken(parent context.Context, account accounts.Account, generation store.CredentialGeneration) (accounts.Account, error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	result, err := s.openAIAccountClient().RefreshAccessToken(ctx, account.Fields)
	if err != nil {
		updates, status := accountTokenRefreshFailureUpdates(err)
		updated, applied, updateErr := s.store.UpdateAccountIfCredentials(account.Token, generation, updates)
		if updateErr != nil {
			message := safeRefreshError(updateErr)
			return account, &protocol.UpstreamError{Status: http.StatusBadGateway, Message: message, Body: message}
		}
		if !applied {
			if updated != nil {
				return openAIAccountFromFields(updated), errOpenAICredentialsChanged
			}
			return account, errOpenAICredentialsChanged
		}
		return account, &protocol.UpstreamError{Status: status, Message: safeRefreshError(err), Body: safeRefreshError(err)}
	}
	updated, applied, err := s.store.RotateAccountTokensIfCredentials(account.Token, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields, generation)
	if err != nil {
		message := safeRefreshError(err)
		return account, &protocol.UpstreamError{Status: http.StatusBadGateway, Message: message, Body: message}
	}
	if !applied {
		if updated != nil {
			return openAIAccountFromFields(updated), errOpenAICredentialsChanged
		}
		return account, errOpenAICredentialsChanged
	}
	return openAIAccountFromFields(updated), nil
}

func accountTokenRefreshFailureUpdates(err error) (map[string]any, int) {
	message := safeRefreshError(err)
	lower := strings.ToLower(message)
	now := time.Now().UTC()
	if terminalTokenRefreshError(lower) {
		return map[string]any{
			"status":                      "异常",
			"status_reason_code":          "account_invalid",
			"last_error_kind":             "auth_invalid",
			"last_token_refresh_error":    message,
			"last_token_refresh_error_at": now.Format(time.RFC3339),
			"next_token_refresh_at":       nil,
		}, http.StatusUnauthorized
	}
	return map[string]any{
		"last_token_refresh_warning":    message,
		"last_token_refresh_warning_at": now.Format(time.RFC3339),
		"next_token_refresh_at":         now.Add(accessTokenRefreshBackoff).Format(time.RFC3339),
	}, http.StatusServiceUnavailable
}

func terminalTokenRefreshError(message string) bool {
	for _, marker := range []string{
		"invalid_grant",
		"invalid_refresh_token",
		"invalid refresh token",
		"refresh token is invalid",
		"refresh_token_invalidated",
		"refresh token has expired",
		"refresh token is expired",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func temporaryTokenRefreshError(message string) error {
	return &protocol.UpstreamError{Status: http.StatusServiceUnavailable, Message: message, Body: message}
}
