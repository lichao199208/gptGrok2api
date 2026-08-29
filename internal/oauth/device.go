package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

const (
	XAIClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIScope    = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
)

type DeviceService struct {
	client    *http.Client
	deviceURL string
	tokenURL  string
	store     *Store
	mu        sync.Mutex
	sessions  map[string]*deviceSession
}

type deviceSession struct {
	ID             string
	DeviceCode     string
	ExpiresAt      int64
	Interval       int
	Verification   string
	CompleteURL    string
	Proxy          string
	LastPollAtUnix int64
}

func NewDeviceService(client *http.Client, deviceURL, tokenURL string, store *Store) *DeviceService {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &DeviceService{client: client, deviceURL: strings.TrimSpace(deviceURL), tokenURL: strings.TrimSpace(tokenURL), store: store, sessions: map[string]*deviceSession{}}
}

func (s *DeviceService) Start(ctx context.Context, proxy string) (map[string]any, error) {
	form := url.Values{"client_id": {XAIClientID}, "scope": {XAIScope}}
	var payload map[string]any
	if err := s.postForm(ctx, s.deviceURL, form, proxy, &payload); err != nil {
		return nil, err
	}
	deviceCode := stringValue(payload["device_code"])
	userCode := stringValue(payload["user_code"])
	if deviceCode == "" || userCode == "" {
		return nil, errors.New("xAI device authorization response is incomplete")
	}
	expires := intValue(payload["expires_in"], 1800)
	if expires < 30 {
		expires = 30
	}
	if expires > 1800 {
		expires = 1800
	}
	interval := intValue(payload["interval"], 5)
	if interval < 1 {
		interval = 1
	}
	if interval > 30 {
		interval = 30
	}
	verification := stringValue(payload["verification_uri"])
	if verification == "" {
		verification = "https://accounts.x.ai/oauth2/device"
	}
	complete := stringValue(payload["verification_uri_complete"])
	if complete == "" {
		complete = verification + "?user_code=" + url.QueryEscape(userCode)
	}
	session := &deviceSession{ID: "xai-device-" + randomID(), DeviceCode: deviceCode, ExpiresAt: time.Now().Unix() + int64(expires), Interval: interval, Verification: verification, CompleteURL: complete, Proxy: proxy}
	s.mu.Lock()
	s.pruneLocked()
	s.sessions[session.ID] = session
	s.mu.Unlock()
	return map[string]any{"id": session.ID, "user_code": userCode, "verification_uri": verification, "verification_uri_complete": complete, "expires_at": session.ExpiresAt, "interval": interval, "status": "pending"}, nil
}

func (s *DeviceService) Poll(ctx context.Context, id string) (map[string]any, error) {
	s.mu.Lock()
	s.pruneLocked()
	session, ok := s.sessions[strings.TrimSpace(id)]
	if ok {
		copy := *session
		session = &copy
	}
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("OAuth device authorization has expired or does not exist")
	}
	if time.Now().Unix() >= session.ExpiresAt {
		return nil, errors.New("OAuth device authorization has expired")
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {session.DeviceCode}, "client_id": {XAIClientID}}
	var payload map[string]any
	status, err := s.postFormStatus(ctx, s.tokenURL, form, session.Proxy, &payload)
	if err != nil {
		return nil, err
	}
	accessToken := stringValue(payload["access_token"])
	if status == http.StatusOK && accessToken != "" {
		account := Account{Email: tokenEmail(accessToken), AccessToken: accessToken, RefreshToken: stringValue(payload["refresh_token"]), IDToken: stringValue(payload["id_token"]), ExpiresAt: time.Now().Unix() + int64(intValue(payload["expires_in"], 21600)), SourceType: "device_authorization"}
		if s.store == nil {
			return nil, errors.New("OAuth store is not configured")
		}
		item, err := s.store.Import(account)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.sessions, session.ID)
		s.mu.Unlock()
		return map[string]any{"status": "authorized", "account": public(item)}, nil
	}
	errorCode := stringValue(payload["error"])
	if errorCode == "authorization_pending" || errorCode == "slow_down" {
		interval := session.Interval
		if errorCode == "slow_down" {
			interval += 5
			if interval > 30 {
				interval = 30
			}
			s.mu.Lock()
			if current := s.sessions[session.ID]; current != nil {
				current.Interval = interval
			}
			s.mu.Unlock()
		}
		return map[string]any{"status": "pending", "interval": interval, "expires_at": session.ExpiresAt}, nil
	}
	s.mu.Lock()
	delete(s.sessions, session.ID)
	s.mu.Unlock()
	description := stringValue(payload["error_description"])
	if errorCode == "expired_token" || errorCode == "access_denied" {
		return nil, fmt.Errorf("xAI device authorization failed: %s", firstNonEmpty(errorCode, description, "authorization_failed"))
	}
	return nil, fmt.Errorf("xAI device authorization failed: %s", firstNonEmpty(errorCode, description, fmt.Sprintf("HTTP %d", status)))
}

func (s *DeviceService) postForm(ctx context.Context, endpoint string, form url.Values, proxy string, target *map[string]any) error {
	_, err := s.postFormStatus(ctx, endpoint, form, proxy, target)
	return err
}

func (s *DeviceService) postFormStatus(ctx context.Context, endpoint string, form url.Values, proxy string, target *map[string]any) (int, error) {
	if strings.TrimSpace(endpoint) == "" {
		return 0, errors.New("OAuth endpoint is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "grok-shell/0.2.93 (linux; x86_64)")
	if proxy != "" {
		req = req.WithContext(proxyruntime.WithURL(req.Context(), proxy))
	}
	response, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return response.StatusCode, readErr
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return response.StatusCode, fmt.Errorf("xAI OAuth returned invalid JSON: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if code := stringValue((*target)["error"]); code != "authorization_pending" && code != "slow_down" {
			return response.StatusCode, fmt.Errorf("xAI OAuth returned HTTP %d: %s", response.StatusCode, firstNonEmpty(code, stringValue((*target)["error_description"]), "request failed"))
		}
	}
	return response.StatusCode, nil
}

func (s *DeviceService) pruneLocked() {
	now := time.Now().Unix()
	for id, session := range s.sessions {
		if session.ExpiresAt <= now {
			delete(s.sessions, id)
		}
	}
}

func tokenEmail(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	for _, key := range []string{"email", "preferred_username", "upn"} {
		if value := stringValue(claims[key]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intValue(value any, fallback int) int {
	if number, err := strconv.Atoi(stringValue(value)); err == nil {
		return number
	}
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
