package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	XAICLIModel   = "grok-4.5"
	XAIClientID   = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIOAuthScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
)

type XAIProbe struct {
	BaseURL  string
	TokenURL string
	HTTP     *http.Client
}

func NewXAIProbe(baseURL, tokenURL string, client *http.Client) *XAIProbe {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &XAIProbe{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), TokenURL: strings.TrimSpace(tokenURL), HTTP: client}
}

type XAIProbeResult struct {
	Status       string
	HTTPStatus   int
	Code         string
	Error        string
	Quota        map[string]any
	AccessToken  string
	RefreshToken string
	IDToken      string
}

func (p *XAIProbe) Test(ctx context.Context, account map[string]any, model, prompt string) XAIProbeResult {
	access, refresh, id := stringValue(account["access_token"]), stringValue(account["refresh_token"]), stringValue(account["id_token"])
	if access == "" {
		return XAIProbeResult{Status: "invalid", Code: "invalid_credentials", Error: "access_token is missing"}
	}
	if strings.TrimSpace(model) == "" {
		model = XAICLIModel
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Reply only OK."
	}
	payload := map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": prompt}}, "stream": false, "max_output_tokens": 64}
	if tokenExpired(access) && refresh != "" {
		if rotated, err := p.refresh(ctx, refresh); err == nil {
			access = rotated.AccessToken
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
		}
	}
	return p.probeWithToken(ctx, access, refresh, id, payload)
}

func (p *XAIProbe) Probe(ctx context.Context, account map[string]any) XAIProbeResult {
	access, refresh, id := stringValue(account["access_token"]), stringValue(account["refresh_token"]), stringValue(account["id_token"])
	if access == "" {
		return XAIProbeResult{Status: "invalid", Code: "invalid_credentials", Error: "access_token is missing"}
	}
	if tokenExpired(access) && refresh != "" {
		if rotated, err := p.refresh(ctx, refresh); err == nil {
			access = rotated.AccessToken
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
		}
	}
	payload := map[string]any{"model": XAICLIModel, "input": []any{map[string]any{"role": "user", "content": "Reply only OK."}}, "stream": false, "max_output_tokens": 8}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	setXAIHeaders(req, access)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code, message := xaiError(body)
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && refresh != "" {
		if rotated, refreshErr := p.refresh(ctx, refresh); refreshErr == nil {
			access = rotated.AccessToken
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
			_ = resp.Body.Close()
			return p.probeWithToken(ctx, access, refresh, id, payload)
		}
	}
	result := XAIProbeResult{HTTPStatus: resp.StatusCode, Code: code, Error: message, AccessToken: access, RefreshToken: refresh, IDToken: id}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Status = "valid"
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Status = "limited"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Status = "invalid"
	default:
		result.Status = "unknown"
	}
	if result.Status == "valid" || result.Status == "limited" {
		result.Quota = p.billing(ctx, access)
	}
	return result
}

func (p *XAIProbe) probeWithToken(ctx context.Context, access, refresh, id string, payload map[string]any) XAIProbeResult {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	setXAIHeaders(req, access)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code, message := xaiError(body)
	result := XAIProbeResult{HTTPStatus: resp.StatusCode, Code: code, Error: message, AccessToken: access, RefreshToken: refresh, IDToken: id}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Status = "valid"
	} else if resp.StatusCode == 429 {
		result.Status = "limited"
	} else if resp.StatusCode == 401 || resp.StatusCode == 403 {
		result.Status = "invalid"
	} else {
		result.Status = "unknown"
	}
	if result.Status == "valid" || result.Status == "limited" {
		result.Quota = p.billing(ctx, access)
	}
	return result
}

func (p *XAIProbe) refresh(ctx context.Context, refreshToken string) (XAITokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {XAIClientID}, "scope": {XAIOAuthScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return XAITokens{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return XAITokens{}, err
	}
	defer resp.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data)
	access := stringValue(data["access_token"])
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || access == "" {
		return XAITokens{}, fmt.Errorf("xAI OAuth refresh HTTP %d: %s", resp.StatusCode, stringValue(data["error_description"]))
	}
	return XAITokens{AccessToken: access, RefreshToken: stringValue(data["refresh_token"]), IDToken: stringValue(data["id_token"])}, nil
}

type XAITokens struct{ AccessToken, RefreshToken, IDToken string }

func (p *XAIProbe) billing(ctx context.Context, access string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/billing", nil)
	if err != nil {
		return nil
	}
	setXAIHeaders(req, access)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil {
		return nil
	}
	used, ok := numberValue(data["creditUsagePercent"])
	if !ok {
		return nil
	}
	return map[string]any{"source": "billing", "used_percent": used, "remaining_percent": 100 - used}
}

func setXAIHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grok-cli/0.2.93 (linux; x86_64)")
}
func xaiError(raw []byte) (string, string) {
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if nested, ok := data["error"].(map[string]any); ok {
		return stringValue(nested["code"]), stringValue(nested["message"])
	}
	return stringValue(data["code"]), xaiFirstNonEmpty(stringValue(data["error"]), stringValue(data["message"]), string(raw))
}
func tokenExpired(token string) bool {
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
	exp, ok := numberValue(claims["exp"])
	return ok && exp > 0 && time.Until(time.Unix(int64(exp), 0)) <= time.Minute
}
func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		v, err := typed.Float64()
		return v, err == nil
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

func xaiFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
