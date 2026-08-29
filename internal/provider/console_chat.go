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

type ConsoleChat struct {
	URL    string
	Client *http.Client
	Proxy  *proxyruntime.Manager
}

func (c *ConsoleChat) SetProxyManager(manager *proxyruntime.Manager) { c.Proxy = manager }

func NewConsoleChat(url string, client *http.Client) *ConsoleChat {
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &ConsoleChat{URL: strings.TrimRight(url, "/"), Client: client}
}

func (c *ConsoleChat) Do(ctx context.Context, account accounts.Account, payload map[string]any) (*http.Response, error) {
	if c.Proxy != nil {
		ctx = proxyruntime.WithURL(ctx, c.Proxy.Resolve(account.Fields, false))
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode console payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	cookie := strings.TrimSpace(stringValue(account.Fields["cookie_header"]))
	if cookie == "" {
		token := strings.TrimPrefix(account.Token, "sso=")
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer anonymous")
	request.Header.Set("Cookie", cookie)
	request.Header.Set("Origin", "https://console.x.ai")
	request.Header.Set("Referer", "https://console.x.ai/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136 Safari/537.36")
	request.Header.Set("x-cluster", "https://us-east-1.api.x.ai")
	return c.Client.Do(request)
}

func ReadConsoleError(response *http.Response) *protocol.UpstreamError {
	defer response.Body.Close()
	buffer := make([]byte, 4096)
	n, _ := response.Body.Read(buffer)
	message := strings.TrimSpace(string(buffer[:n]))
	if message == "" {
		message = fmt.Sprintf("Console upstream returned HTTP %d", response.StatusCode)
	}
	return &protocol.UpstreamError{Status: response.StatusCode, Message: message, Body: message}
}

func ConsoleTimeout(seconds time.Duration) time.Duration {
	if seconds <= 0 {
		return 120 * time.Second
	}
	return seconds
}
