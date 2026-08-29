package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	OpenAIOAuthClientID = "app_2SKx67EdpoN0G6j64fRvigXD"
	OpenAIOAuthAudience = "https://api.openai.com/v1"
	OpenAIOAuthRedirect = "https://platform.openai.com/auth/callback"
	OpenAIAuth0Client   = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
)

var ErrOAuthLogin = errors.New("openai oauth login failed")

type OpenAILogin struct {
	AuthBaseURL string
	PlatformURL string
	TokenURL    string
	HTTP        *http.Client
	TTL         time.Duration
	mu          sync.Mutex
	sessions    map[string]loginSession
}

type loginSession struct {
	Verifier    string
	State       string
	RedirectURI string
	CreatedAt   time.Time
}

func NewOpenAILogin(authBaseURL, platformURL, tokenURL string, client *http.Client) *OpenAILogin {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &OpenAILogin{
		AuthBaseURL: strings.TrimRight(strings.TrimSpace(authBaseURL), "/"),
		PlatformURL: strings.TrimRight(strings.TrimSpace(platformURL), "/"),
		TokenURL:    strings.TrimSpace(tokenURL),
		HTTP:        client,
		TTL:         10 * time.Minute,
		sessions:    map[string]loginSession{},
	}
}

func (s *OpenAILogin) Start(emailHint string) (map[string]string, error) {
	if s.AuthBaseURL == "" || s.PlatformURL == "" || s.TokenURL == "" {
		return nil, fmt.Errorf("%w: oauth endpoints are not configured", ErrOAuthLogin)
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, fmt.Errorf("%w: generate PKCE: %v", ErrOAuthLogin, err)
	}
	sessionID, err := randomURL(24)
	if err != nil {
		return nil, fmt.Errorf("%w: generate session: %v", ErrOAuthLogin, err)
	}
	nonce, err := randomURL(24)
	if err != nil {
		return nil, fmt.Errorf("%w: generate nonce: %v", ErrOAuthLogin, err)
	}
	stateNonce, err := randomURL(16)
	if err != nil {
		return nil, fmt.Errorf("%w: generate state: %v", ErrOAuthLogin, err)
	}
	state := sessionID + "." + stateNonce
	deviceID, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("%w: generate device id: %v", ErrOAuthLogin, err)
	}
	params := url.Values{
		"issuer": {s.AuthBaseURL}, "client_id": {OpenAIOAuthClientID},
		"audience": {OpenAIOAuthAudience}, "redirect_uri": {OpenAIOAuthRedirect},
		"device_id": {deviceID}, "screen_hint": {"login_or_signup"}, "max_age": {"0"},
		"scope": {"openid profile email offline_access"}, "response_type": {"code"},
		"response_mode": {"query"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"auth0Client": {OpenAIAuth0Client},
	}
	if hint := strings.TrimSpace(emailHint); hint != "" {
		params.Set("login_hint", hint)
	}
	s.mu.Lock()
	s.purgeLocked(time.Now())
	s.sessions[sessionID] = loginSession{Verifier: verifier, State: state, RedirectURI: OpenAIOAuthRedirect, CreatedAt: time.Now()}
	s.mu.Unlock()
	return map[string]string{
		"session_id":          sessionID,
		"authorize_url":       s.AuthBaseURL + "/api/accounts/authorize?" + params.Encode(),
		"expires_in":          fmt.Sprintf("%d", int((10*time.Minute)/time.Second)),
		"redirect_uri_prefix": OpenAIOAuthRedirect,
	}, nil
}

func (s *OpenAILogin) Finish(ctx context.Context, sessionID, callback string) (map[string]string, error) {
	code, state, err := extractCallback(callback)
	if err != nil {
		return nil, err
	}
	stateSession := ""
	if state != "" {
		stateSession = strings.SplitN(state, ".", 2)[0]
	}
	candidates := []string{stateSession, strings.TrimSpace(sessionID)}
	s.mu.Lock()
	s.purgeLocked(time.Now())
	var picked string
	var session loginSession
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if current, ok := s.sessions[candidate]; ok {
			picked, session = candidate, current
			break
		}
	}
	s.mu.Unlock()
	if picked == "" {
		return nil, fmt.Errorf("%w: oauth session expired or not found", ErrOAuthLogin)
	}
	if state != "" && state != session.State {
		return nil, fmt.Errorf("%w: state mismatch", ErrOAuthLogin)
	}
	tokens, err := s.exchange(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.sessions, picked)
	s.mu.Unlock()
	return tokens, nil
}

func (s *OpenAILogin) exchange(ctx context.Context, code, verifier, redirectURI string) (map[string]string, error) {
	body := map[string]string{"client_id": OpenAIOAuthClientID, "code_verifier": verifier, "grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: create token request: %v", ErrOAuthLogin, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", s.PlatformURL)
	req.Header.Set("Referer", s.PlatformURL+"/")
	req.Header.Set("Auth0-Client", OpenAIAuth0Client)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: token request: %v", ErrOAuthLogin, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	access := loginStringValue(payload, "access_token")
	refresh := loginStringValue(payload, "refresh_token")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || access == "" || refresh == "" {
		detail := loginStringValue(payload, "error_description", "error", "message")
		if detail == "" {
			detail = strings.TrimSpace(string(data))
		}
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("%w: token endpoint HTTP %d: %s", ErrOAuthLogin, resp.StatusCode, detail)
	}
	return map[string]string{"access_token": access, "refresh_token": refresh, "id_token": loginStringValue(payload, "id_token")}, nil
}

func (s *OpenAILogin) purgeLocked(now time.Time) {
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	for id, item := range s.sessions {
		if now.Sub(item.CreatedAt) > ttl {
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) <= 64 {
		return
	}
	for id, item := range s.sessions {
		if len(s.sessions) <= 64 {
			break
		}
		oldest := true
		for other, value := range s.sessions {
			if other != id && value.CreatedAt.Before(item.CreatedAt) {
				oldest = false
				break
			}
		}
		if oldest {
			delete(s.sessions, id)
		}
	}
}

func pkce() (string, string, error) {
	verifier, err := randomURL(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomURL(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func extractCallback(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", fmt.Errorf("%w: callback or code is required", ErrOAuthLogin)
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("%w: invalid callback URL", ErrOAuthLogin)
		}
		query := parsed.Query()
		code := strings.TrimSpace(query.Get("code"))
		if code == "" {
			detail := query.Get("error_description")
			if detail == "" {
				detail = query.Get("error")
			}
			if detail == "" {
				detail = "callback URL has no code"
			}
			return "", "", fmt.Errorf("%w: %s", ErrOAuthLogin, detail)
		}
		return code, strings.TrimSpace(query.Get("state")), nil
	}
	return raw, "", nil
}

func loginStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
