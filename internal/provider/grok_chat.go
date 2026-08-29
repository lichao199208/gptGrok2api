package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type GrokChat struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
	Proxy   *proxyruntime.Manager
}

func (g *GrokChat) SetProxyManager(manager *proxyruntime.Manager) { g.Proxy = manager }

func NewGrokChat(url string, client *http.Client, timeout time.Duration) *GrokChat {
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &GrokChat{URL: strings.TrimRight(url, "/"), Client: client, Timeout: timeout}
}

func (g *GrokChat) Do(ctx context.Context, account accounts.Account, payload map[string]any) (*http.Response, error) {
	if g.Proxy != nil {
		ctx = proxyruntime.WithURL(ctx, g.Proxy.Resolve(account.Fields, false))
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode grok payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for key, value := range headers(account) {
		req.Header.Set(key, value)
	}
	return g.Client.Do(req)
}

func ReadError(response *http.Response) *protocol.UpstreamError {
	defer response.Body.Close()
	buffer := make([]byte, 4096)
	n, _ := response.Body.Read(buffer)
	message := strings.TrimSpace(string(buffer[:n]))
	if message == "" {
		message = fmt.Sprintf("Grok upstream returned HTTP %d", response.StatusCode)
	}
	return &protocol.UpstreamError{
		Status:  response.StatusCode,
		Message: message,
		Body:    strings.TrimSpace(message),
	}
}

func headers(account accounts.Account) map[string]string {
	cookie := stringValue(account.Fields["cookie_header"])
	if cookie == "" {
		token := strings.TrimPrefix(account.Token, "sso=")
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	return map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate, br",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		"Content-Type":    "application/json",
		"Origin":          "https://grok.com",
		"Referer":         "https://grok.com/",
		"User-Agent":      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136 Safari/537.36",
		"Cookie":          cookie,
	}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
