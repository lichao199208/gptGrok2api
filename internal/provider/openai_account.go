package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

const (
	openAIOAuthClientID = "app_2SKx67EdpoN0G6j64fRvigXD"
	openAIUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	openAIClientVersion = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	openAIClientBuild   = "6708908"
)

var ErrInvalidAccessToken = errors.New("openai access token is invalid")

// AccountRefreshResult contains only fields returned by ChatGPT Web and the
// rotated credentials. Callers decide which fields are safe to expose.
type AccountRefreshResult struct {
	Fields       map[string]any
	AccessToken  string
	RefreshToken string
	IDToken      string
}

type OpenAIAccountClient struct {
	BaseURL          string
	OAuthURL         string
	HTTP             *http.Client
	Proxy            *proxyruntime.Manager
	FlareSolverrURL  string
	ClearanceEnabled bool
	ClearanceTimeout time.Duration
	clearanceMu      sync.Mutex
	clearance        map[string]clearanceBundle
	browserMu        sync.Mutex
	browsers         map[string]*browserHTTP
}

func NewOpenAIAccountClient(baseURL, oauthURL string, client *http.Client, proxy *proxyruntime.Manager, clearance ...ClearanceConfig) *OpenAIAccountClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	settings := ClearanceConfig{}
	if len(clearance) > 0 {
		settings = clearance[0]
	}
	return &OpenAIAccountClient{
		BaseURL:          strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		OAuthURL:         strings.TrimSpace(oauthURL),
		HTTP:             client,
		Proxy:            proxy,
		FlareSolverrURL:  strings.TrimRight(strings.TrimSpace(settings.URL), "/"),
		ClearanceEnabled: settings.Enabled,
		ClearanceTimeout: settings.Timeout,
		clearance:        map[string]clearanceBundle{},
		browsers:         map[string]*browserHTTP{},
	}
}

// ClearanceConfig controls the optional Cloudflare clearance provider used by
// ChatGPT Web requests. It is intentionally opt-in because FlareSolverr is an
// external browser service and is not required for every deployment.
type ClearanceConfig struct {
	URL     string
	Enabled bool
	Timeout time.Duration
}

type clearanceBundle struct {
	Cookie    string
	UserAgent string
}

type flareSolverrCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type flareSolverrResponse struct {
	Status   string `json:"status"`
	Solution struct {
		Cookies   []flareSolverrCookie `json:"cookies"`
		UserAgent string               `json:"userAgent"`
		Response  string               `json:"response"`
	} `json:"solution"`
}

func (c *OpenAIAccountClient) RefreshAccount(ctx context.Context, account map[string]any) (AccountRefreshResult, error) {
	accessToken := aiString(account, "access_token", "accessToken", "token")
	if accessToken == "" {
		return AccountRefreshResult{}, errors.New("access_token is required")
	}
	activeToken := accessToken
	rotated := AccountRefreshResult{AccessToken: accessToken}

	// Browser sessions are not Platform OAuth credentials. They can still be
	// retried after a 401, but must not be proactively sent to oauth.openai.com.
	if !strings.EqualFold(aiString(account, "source_type"), "chatgpt_web") && aiString(account, "refresh_token") != "" && tokenNeedsRefresh(activeToken) {
		if tokens, err := c.refreshOAuth(ctx, aiString(account, "refresh_token"), account); err == nil {
			activeToken = tokens.AccessToken
			rotated.AccessToken = tokens.AccessToken
			rotated.RefreshToken = tokens.RefreshToken
			rotated.IDToken = tokens.IDToken
		}
	}

	fields, err := c.fetchUserInfo(ctx, activeToken, account)
	if errors.Is(err, ErrInvalidAccessToken) && aiString(account, "refresh_token") != "" {
		tokens, refreshErr := c.refreshOAuth(ctx, aiString(account, "refresh_token"), account)
		if refreshErr != nil {
			return AccountRefreshResult{}, fmt.Errorf("access token invalid and oauth refresh failed: %w", refreshErr)
		}
		activeToken = tokens.AccessToken
		rotated.AccessToken = tokens.AccessToken
		rotated.RefreshToken = tokens.RefreshToken
		rotated.IDToken = tokens.IDToken
		fields, err = c.fetchUserInfo(ctx, activeToken, account)
	}
	if err != nil {
		return AccountRefreshResult{}, err
	}
	rotated.Fields = fields
	return rotated, nil
}

// RefreshAccessToken rotates an account's OAuth credential set immediately,
// then verifies the new access token against ChatGPT. It is used by the admin
// bulk AT action instead of waiting for the old JWT to fail a normal request.
func (c *OpenAIAccountClient) RefreshAccessToken(ctx context.Context, account map[string]any) (AccountRefreshResult, error) {
	refreshToken := aiString(account, "refresh_token")
	if refreshToken == "" {
		return AccountRefreshResult{}, errors.New("账号缺少 refresh_token，需先完成一次 GPT 协议登录")
	}
	rotated, err := c.refreshOAuth(ctx, refreshToken, account)
	if err != nil {
		return AccountRefreshResult{}, err
	}
	fields, err := c.fetchUserInfo(ctx, rotated.AccessToken, account)
	if err != nil {
		return AccountRefreshResult{}, fmt.Errorf("新 access token 验证失败: %w", err)
	}
	rotated.Fields = fields
	return rotated, nil
}

func (c *OpenAIAccountClient) fetchUserInfo(ctx context.Context, accessToken string, account map[string]any) (map[string]any, error) {
	me, err := c.getMe(ctx, accessToken, account)
	if err != nil {
		return nil, err
	}
	init, err := c.getConversationInit(ctx, accessToken, account)
	if err != nil {
		return nil, err
	}
	defaultAccount, err := c.getDefaultAccount(ctx, accessToken, account)
	if err != nil {
		return nil, err
	}
	limits, _ := init["limits_progress"].([]any)
	quota, restoreAt, unknown := extractImageQuota(limits)
	planType := aiString(defaultAccount, "plan_type")
	if planType == "" {
		planType = "free"
	}
	status := "正常"
	if !(unknown && !strings.EqualFold(planType, "free")) && quota == 0 {
		status = "限流"
	}
	return map[string]any{
		"email":               me["email"],
		"user_id":             me["id"],
		"type":                planType,
		"quota":               quota,
		"image_quota_unknown": unknown,
		"limits_progress":     limits,
		"default_model_slug":  init["default_model_slug"],
		"restore_at":          restoreAt,
		"status":              status,
	}, nil
}

func (c *OpenAIAccountClient) getMe(ctx context.Context, token string, account map[string]any) (map[string]any, error) {
	return c.getJSON(ctx, http.MethodGet, "/backend-api/me", token, account, nil)
}

func (c *OpenAIAccountClient) getConversationInit(ctx context.Context, token string, account map[string]any) (map[string]any, error) {
	return c.getJSON(ctx, http.MethodPost, "/backend-api/conversation/init", token, account, map[string]any{
		"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480,
	})
}

func (c *OpenAIAccountClient) getDefaultAccount(ctx context.Context, token string, account map[string]any) (map[string]any, error) {
	value, err := c.getJSON(ctx, http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", token, account, nil)
	if err != nil {
		return nil, err
	}
	accounts, _ := value["accounts"].(map[string]any)
	defaultValue, _ := accounts["default"].(map[string]any)
	result, _ := defaultValue["account"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	return result, nil
}

func (c *OpenAIAccountClient) getJSON(ctx context.Context, method, path, token string, account map[string]any, body any) (map[string]any, error) {
	var rawBody []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rawBody = raw
	}
	// ChatGPT's target headers contain the route path, not its query string.
	// The Python client follows the same convention for the account endpoint.
	targetPath := path
	if parsed, parseErr := url.Parse(path); parseErr == nil && parsed.Path != "" {
		targetPath = parsed.Path
	}
	proxyURL := c.ProxyURL(account)
	makeRequest := func(bundle clearanceBundle) (*http.Response, error) {
		var reader io.Reader
		if rawBody != nil {
			reader = strings.NewReader(string(rawBody))
		}
		req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, proxyURL), method, c.BaseURL+path, reader)
		if err != nil {
			return nil, err
		}
		setOpenAIHeaders(req, targetPath, token, account)
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if bundle.Cookie != "" {
			req.Header.Set("Cookie", bundle.Cookie)
		}
		if bundle.UserAgent != "" {
			req.Header.Set("User-Agent", bundle.UserAgent)
		}
		return c.doChatGPT(req, proxyURL)
	}
	response, err := makeRequest(c.cachedClearance(proxyURL))
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusForbidden && c.ClearanceEnabled && c.FlareSolverrURL != "" {
		_ = response.Body.Close()
		if bundle, clearanceErr := c.refreshClearance(ctx, proxyURL, method, c.BaseURL+path, token, account, targetPath); clearanceErr == nil {
			response, err = makeRequest(bundle)
			if err != nil {
				return nil, err
			}
		}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, ErrInvalidAccessToken
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("openai account endpoint %s returned HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return nil, fmt.Errorf("decode OpenAI account response %s: %w", path, err)
	}
	return value, nil
}

func (c *OpenAIAccountClient) doChatGPT(req *http.Request, proxyURL string) (*http.Response, error) {
	if !browserEligible(c.BaseURL) {
		return c.HTTP.Do(req)
	}
	browser, err := c.browserFor(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create Chrome TLS client: %w", err)
	}
	return browser.Do(req)
}

func (c *OpenAIAccountClient) browserFor(proxyURL string) (*browserHTTP, error) {
	c.browserMu.Lock()
	defer c.browserMu.Unlock()
	if browser := c.browsers[proxyURL]; browser != nil {
		return browser, nil
	}
	timeout := c.HTTP.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	browser, err := newBrowserHTTP(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	c.browsers[proxyURL] = browser
	return browser, nil
}

func browserEligible(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

func (c *OpenAIAccountClient) cachedClearance(proxyURL string) clearanceBundle {
	c.clearanceMu.Lock()
	defer c.clearanceMu.Unlock()
	return c.clearance[proxyURL]
}

func (c *OpenAIAccountClient) refreshClearance(ctx context.Context, proxyURL, method, targetURL, token string, account map[string]any, targetPath string) (clearanceBundle, error) {
	c.clearanceMu.Lock()
	defer c.clearanceMu.Unlock()
	if bundle := c.clearance[proxyURL]; bundle.Cookie != "" || bundle.UserAgent != "" {
		return bundle, nil
	}
	timeout := c.ClearanceTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := "request.get"
	if strings.EqualFold(method, http.MethodPost) {
		command = "request.post"
	}
	payload := map[string]any{
		"cmd":        command,
		"url":        targetURL,
		"maxTimeout": timeout.Milliseconds(),
	}
	// Ask the browser service to open the protected endpoint itself. Opening
	// only the public homepage can report "challenge not detected" while the
	// backend API still returns a Cloudflare 403.
	probe := &http.Request{Header: make(http.Header)}
	setOpenAIHeaders(probe, targetPath, token, account)
	flareHeaders := map[string]string{}
	for key, values := range probe.Header {
		if len(values) > 0 && key != "Cookie" {
			flareHeaders[key] = values[0]
		}
	}
	payload["headers"] = flareHeaders
	if proxyURL != "" {
		payload["proxy"] = map[string]string{"url": proxyURL}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return clearanceBundle{}, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.FlareSolverrURL+"/v1", strings.NewReader(string(raw)))
	if err != nil {
		return clearanceBundle{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// FlareSolverr is an internal control-plane service. It must be reached
	// directly; sending this request through the account's outbound proxy makes
	// names such as `flaresolverr` impossible to resolve and never starts a
	// clearance job.
	clearanceClient := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{},
	}
	response, err := clearanceClient.Do(req)
	if err != nil {
		return clearanceBundle{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return clearanceBundle{}, fmt.Errorf("flaresolverr HTTP %d", response.StatusCode)
	}
	var result flareSolverrResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return clearanceBundle{}, fmt.Errorf("decode flaresolverr response: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(result.Status), "ok") {
		return clearanceBundle{}, errors.New("flaresolverr did not return a solution")
	}
	parts := make([]string, 0, len(result.Solution.Cookies))
	for _, cookie := range result.Solution.Cookies {
		if strings.TrimSpace(cookie.Name) != "" {
			parts = append(parts, strings.TrimSpace(cookie.Name)+"="+cookie.Value)
		}
	}
	bundle := clearanceBundle{Cookie: strings.Join(parts, "; "), UserAgent: strings.TrimSpace(result.Solution.UserAgent)}
	if bundle.Cookie == "" && bundle.UserAgent == "" {
		detail := strings.TrimSpace(result.Solution.Response)
		if detail == "" {
			detail = "flaresolverr solution has no cookie or user-agent"
		}
		return clearanceBundle{}, errors.New(detail)
	}
	c.clearance[proxyURL] = bundle
	return bundle, nil
}

func (c *OpenAIAccountClient) refreshOAuth(ctx context.Context, refreshToken string, account map[string]any) (AccountRefreshResult, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", openAIOAuthClientID)
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, c.ProxyURL(account)), http.MethodPost, c.OAuthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return AccountRefreshResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", openAIUserAgent)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return AccountRefreshResult{}, err
	}
	defer response.Body.Close()
	var value map[string]any
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&value)
	accessToken := aiString(value, "access_token")
	if response.StatusCode != http.StatusOK || accessToken == "" {
		detail := aiString(value, "error_description", "error", "message")
		return AccountRefreshResult{}, fmt.Errorf("oauth refresh HTTP %d: %s", response.StatusCode, detail)
	}
	return AccountRefreshResult{AccessToken: accessToken, RefreshToken: aiString(value, "refresh_token"), IDToken: aiString(value, "id_token")}, nil
}

func (c *OpenAIAccountClient) ProxyURL(account map[string]any) string {
	if c.Proxy == nil {
		return ""
	}
	return c.Proxy.Resolve(account, false)
}

func setOpenAIHeaders(req *http.Request, path, token string, account map[string]any) {
	fingerprint := buildOpenAIFingerprint(account)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", fingerprint.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Sec-CH-UA", fingerprint.SecCHUA)
	req.Header.Set("Sec-CH-UA-Arch", `"x86"`)
	req.Header.Set("Sec-CH-UA-Bitness", `"64"`)
	req.Header.Set("Sec-CH-UA-Full-Version", fingerprint.SecCHUAFullVersion)
	req.Header.Set("Sec-CH-UA-Full-Version-List", fingerprint.SecCHUAFullVersionList)
	req.Header.Set("Sec-CH-UA-Mobile", fingerprint.SecCHUAMobile)
	req.Header.Set("Sec-CH-UA-Model", `""`)
	req.Header.Set("Sec-CH-UA-Platform", fingerprint.SecCHUAPlatform)
	req.Header.Set("Sec-CH-UA-Platform-Version", `"19.0.0"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("OAI-Device-Id", fingerprint.DeviceID)
	req.Header.Set("OAI-Session-Id", fingerprint.SessionID)
	req.Header.Set("OAI-Language", "zh-CN")
	req.Header.Set("OAI-Client-Version", openAIClientVersion)
	req.Header.Set("OAI-Client-Build-Number", openAIClientBuild)
	req.Header.Set("X-OpenAI-Target-Path", path)
	req.Header.Set("X-OpenAI-Target-Route", path)
	if cookie := aiString(account, "cookie_header", "cookie", "cookies"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func extractImageQuota(items []any) (int, any, bool) {
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || aiString(item, "feature_name") != "image_gen" {
			continue
		}
		quota := intValue(item["remaining"])
		return quota, item["reset_after"], false
	}
	return 0, nil, true
}

// AccessTokenNeedsRefresh reports whether a JWT access token expires within the
// next 24 hours. Opaque browser/session tokens are deliberately left alone.
func AccessTokenNeedsRefresh(token string) bool {
	return tokenNeedsRefresh(token)
}
func tokenNeedsRefresh(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	exp, ok := claims["exp"].(float64)
	return ok && exp > 0 && time.Until(time.Unix(int64(exp), 0)) <= 24*time.Hour
}

func aiString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		var parsed int
		_, _ = fmt.Sscanf(fmt.Sprint(value), "%d", &parsed)
		return parsed
	}
}

func firstUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		// RFC 4122 version 4 / variant 1.
		value[6] = (value[6] & 0x0f) | 0x40
		value[8] = (value[8] & 0x3f) | 0x80
		return fmt.Sprintf("%s-%s-%s-%s-%s",
			hex.EncodeToString(value[0:4]),
			hex.EncodeToString(value[4:6]),
			hex.EncodeToString(value[6:8]),
			hex.EncodeToString(value[8:10]),
			hex.EncodeToString(value[10:16]))
	}
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
}

type openAIFingerprint struct {
	UserAgent              string
	DeviceID               string
	SessionID              string
	SecCHUA                string
	SecCHUAFullVersion     string
	SecCHUAFullVersionList string
	SecCHUAMobile          string
	SecCHUAPlatform        string
}

func buildOpenAIFingerprint(account map[string]any) openAIFingerprint {
	// Keep the defaults aligned with the Python curl_cffi client. An imported
	// account may carry a more precise browser fingerprint under fp.
	fingerprint := openAIFingerprint{
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0",
		DeviceID:               firstUUID(),
		SessionID:              firstUUID(),
		SecCHUA:                `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		SecCHUAFullVersion:     `"143.0.3650.96"`,
		SecCHUAFullVersionList: `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`,
		SecCHUAMobile:          "?0",
		SecCHUAPlatform:        `"Windows"`,
	}
	values := map[string]string{}
	for key, value := range account {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			values[strings.ToLower(strings.ReplaceAll(key, "_", "-"))] = strings.TrimSpace(text)
		}
	}
	if raw, ok := account["fp"].(map[string]any); ok {
		for key, value := range raw {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				values[strings.ToLower(strings.ReplaceAll(key, "_", "-"))] = strings.TrimSpace(text)
			}
		}
	}
	if value := firstFingerprintValue(values, "user-agent"); value != "" {
		fingerprint.UserAgent = value
	}
	if value := firstFingerprintValue(values, "oai-device-id"); value != "" {
		fingerprint.DeviceID = value
	}
	if value := firstFingerprintValue(values, "oai-session-id"); value != "" {
		fingerprint.SessionID = value
	}
	if value := firstFingerprintValue(values, "sec-ch-ua"); value != "" {
		fingerprint.SecCHUA = value
	}
	if value := firstFingerprintValue(values, "sec-ch-ua-full-version"); value != "" {
		fingerprint.SecCHUAFullVersion = value
	}
	if value := firstFingerprintValue(values, "sec-ch-ua-full-version-list"); value != "" {
		fingerprint.SecCHUAFullVersionList = value
	}
	if value := firstFingerprintValue(values, "sec-ch-ua-mobile"); value != "" {
		fingerprint.SecCHUAMobile = value
	}
	if value := firstFingerprintValue(values, "sec-ch-ua-platform"); value != "" {
		fingerprint.SecCHUAPlatform = value
	}
	return fingerprint
}

func firstFingerprintValue(values map[string]string, key string) string {
	if value := strings.TrimSpace(values[key]); value != "" {
		return value
	}
	return ""
}
