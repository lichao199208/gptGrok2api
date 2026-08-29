package register

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPDrivers struct {
	MailURL    string
	CaptchaURL string
	DriverURL  string
	APIKey     string
	HTTP       *http.Client
}

func NewHTTPDrivers(mailURL, captchaURL, driverURL, apiKey string, client *http.Client) *HTTPDrivers {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &HTTPDrivers{MailURL: strings.TrimRight(mailURL, "/"), CaptchaURL: strings.TrimRight(captchaURL, "/"), DriverURL: strings.TrimRight(driverURL, "/"), APIKey: strings.TrimSpace(apiKey), HTTP: client}
}

func (d *HTTPDrivers) RequestCode(ctx context.Context, email string) (string, error) {
	value, err := d.post(ctx, d.MailURL, map[string]any{"email": email})
	if err != nil {
		return "", err
	}
	code := first(value, "code", "verification_code", "otp")
	if code == "" {
		return "", fmt.Errorf("mail driver returned no verification code")
	}
	return code, nil
}

func (d *HTTPDrivers) Solve(ctx context.Context, target string) (string, error) {
	value, err := d.post(ctx, d.CaptchaURL, map[string]any{"target": target})
	if err != nil {
		return "", err
	}
	token := first(value, "token", "captcha_token", "solution")
	if token == "" {
		return "", fmt.Errorf("captcha driver returned no token")
	}
	return token, nil
}

func (d *HTTPDrivers) Register(ctx context.Context, request RegistrationRequest, code, captcha string) (RegistrationResult, error) {
	value, err := d.post(ctx, d.DriverURL, map[string]any{"target": request.Target, "email": request.Email, "verification_code": code, "captcha_token": captcha})
	if err != nil {
		return RegistrationResult{}, err
	}
	data, _ := value["account"].(map[string]any)
	if data == nil {
		data = value
	}
	return RegistrationResult{Email: first(data, "email"), SSO: first(data, "sso", "access_token", "token"), Status: first(data, "status"), Data: data}, nil
}

func (d *HTTPDrivers) post(ctx context.Context, endpoint string, payload map[string]any) (map[string]any, error) {
	if endpoint == "" {
		return nil, ErrExecutorNotConfigured
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if d.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.APIKey)
		req.Header.Set("X-API-Key", d.APIKey)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var value map[string]any
	_ = json.Unmarshal(body, &value)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("registration driver HTTP %d: %s", resp.StatusCode, first(value, "error", "message"))
	}
	return value, nil
}

func first(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := strings.TrimSpace(fmt.Sprint(value[key])); text != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}
