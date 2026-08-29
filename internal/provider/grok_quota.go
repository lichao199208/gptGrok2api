package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

var ErrGrokInvalidCredentials = errors.New("grok credentials are invalid")

type QuotaWindow struct {
	Remaining     int   `json:"remaining"`
	Total         int   `json:"total"`
	WindowSeconds int   `json:"window_seconds"`
	ResetAt       int64 `json:"reset_at"`
}

type GrokQuota struct {
	URL    string
	Client *http.Client
	Proxy  *proxyruntime.Manager
}

func NewGrokQuota(url string, client *http.Client, proxy *proxyruntime.Manager) *GrokQuota {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GrokQuota{URL: strings.TrimRight(strings.TrimSpace(url), "/"), Client: client, Proxy: proxy}
}

var grokQuotaModes = map[string]string{
	"auto": "auto", "fast": "fast", "expert": "expert", "heavy": "heavy", "grok_4_3": "grok-420-computer-use-sa",
}

func (q *GrokQuota) Refresh(ctx context.Context, account accounts.Account) (map[string]QuotaWindow, error) {
	return q.RefreshToken(ctx, account.Token, account.Fields)
}

func (q *GrokQuota) RefreshToken(ctx context.Context, token string, fields map[string]any) (map[string]QuotaWindow, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "sso="))
	if token == "" {
		return nil, errors.New("sso token is required")
	}
	type result struct {
		mode string
		win  QuotaWindow
		err  error
	}
	results := make(chan result, len(grokQuotaModes))
	var wg sync.WaitGroup
	for mode, modelName := range grokQuotaModes {
		wg.Add(1)
		go func(mode, modelName string) {
			defer wg.Done()
			win, err := q.fetch(ctx, token, fields, modelName)
			results <- result{mode: mode, win: win, err: err}
		}(mode, modelName)
	}
	wg.Wait()
	close(results)
	quotas := map[string]QuotaWindow{}
	var firstErr error
	for item := range results {
		if item.err != nil {
			if errors.Is(item.err, ErrGrokInvalidCredentials) {
				return nil, item.err
			}
			if firstErr == nil {
				firstErr = item.err
			}
			continue
		}
		quotas[item.mode] = item.win
	}
	if len(quotas) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return quotas, nil
}

func (q *GrokQuota) ProbeFast(ctx context.Context, token string, fields map[string]any) (QuotaWindow, error) {
	return q.fetch(ctx, strings.TrimSpace(strings.TrimPrefix(token, "sso=")), fields, "fast")
}

func (q *GrokQuota) fetch(ctx context.Context, token string, fields map[string]any, modelName string) (QuotaWindow, error) {
	body, err := json.Marshal(map[string]string{"modelName": modelName})
	if err != nil {
		return QuotaWindow{}, err
	}
	proxyURL := ""
	if q.Proxy != nil {
		proxyURL = q.Proxy.Resolve(fields, false)
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, proxyURL), http.MethodPost, q.URL, bytes.NewReader(body))
	if err != nil {
		return QuotaWindow{}, err
	}
	setGrokQuotaHeaders(req, token, fields)
	response, err := q.Client.Do(req)
	if err != nil {
		return QuotaWindow{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusUnauthorized || isGrokInvalidBody(string(raw)) {
		return QuotaWindow{}, ErrGrokInvalidCredentials
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return QuotaWindow{}, fmt.Errorf("grok rate-limits HTTP %d", response.StatusCode)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return QuotaWindow{}, fmt.Errorf("decode grok rate-limits response: %w", err)
	}
	remaining, ok := quotaNumber(value["remainingQueries"])
	if !ok {
		return QuotaWindow{}, errors.New("grok rate-limits response missing remainingQueries")
	}
	total, ok := quotaNumber(value["totalQueries"])
	if !ok {
		total = remaining
	}
	window, ok := quotaNumber(value["windowSizeSeconds"])
	if !ok || window <= 0 {
		window = 72000
	}
	return QuotaWindow{Remaining: maxInt(0, remaining), Total: maxInt(0, total), WindowSeconds: window, ResetAt: time.Now().Add(time.Duration(window) * time.Second).UnixMilli()}, nil
}

func setGrokQuotaHeaders(req *http.Request, token string, fields map[string]any) {
	cookie := stringValue(fields["cookie_header"])
	if cookie == "" {
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://grok.com")
	req.Header.Set("Referer", "https://grok.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136 Safari/537.36")
}

func isGrokInvalidBody(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{"invalid-credentials", "bad-credentials", "session not found", "token revoked", "token expired", "account suspended", "blocked-user"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func quotaNumber(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		var parsed int
		_, err := fmt.Sscanf(fmt.Sprint(value), "%d", &parsed)
		return parsed, err == nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
