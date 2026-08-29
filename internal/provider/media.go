package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type Media struct {
	Client         *http.Client
	ImageChatURL   string
	MediaPostURL   string
	AssetUploadURL string
	AssetsBaseURL  string
	RequestTimeout time.Duration
	Proxy          *proxyruntime.Manager
}

func (m *Media) SetProxyManager(manager *proxyruntime.Manager) { m.Proxy = manager }

func NewMedia(client *http.Client, imageChatURL, mediaPostURL, assetUploadURL, assetsBaseURL string, timeout time.Duration) *Media {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Media{
		Client:         client,
		ImageChatURL:   strings.TrimRight(imageChatURL, "/"),
		MediaPostURL:   strings.TrimRight(mediaPostURL, "/"),
		AssetUploadURL: strings.TrimRight(assetUploadURL, "/"),
		AssetsBaseURL:  strings.TrimRight(assetsBaseURL, "/"),
		RequestTimeout: timeout,
	}
}

func (m *Media) StreamChat(ctx context.Context, account accounts.Account, payload map[string]any) (*http.Response, error) {
	return m.doJSON(ctx, http.MethodPost, m.ImageChatURL, account, payload, true)
}

func (m *Media) CreatePost(ctx context.Context, account accounts.Account, mediaType, mediaURL, prompt string) (map[string]any, error) {
	return m.doJSONMap(ctx, http.MethodPost, m.MediaPostURL, account, protocol.BuildMediaPostPayload(mediaType, mediaURL, prompt))
}

func (m *Media) Upload(ctx context.Context, account accounts.Account, filename, mime, encoded string) (string, string, error) {
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return "", "", fmt.Errorf("invalid base64 content: %w", err)
	}
	body, err := m.doJSONMap(ctx, http.MethodPost, m.AssetUploadURL, account, map[string]any{
		"fileName": filename, "fileMimeType": mime, "content": encoded,
	})
	if err != nil {
		return "", "", err
	}
	return firstString(body["fileMetadataId"], firstString(body["fileId"], "")), firstString(body["fileUri"], ""), nil
}

func (m *Media) Fetch(ctx context.Context, account accounts.Account, filePath string) ([]byte, string, error) {
	if m.Proxy != nil {
		ctx = proxyruntime.WithURL(ctx, m.Proxy.Resolve(account.Fields, true))
	}
	urlValue, origin := protocol.ResolveDownloadURL(filePath)
	if strings.HasPrefix(urlValue, "https://assets.grok.com/") && m.AssetsBaseURL != "" {
		urlValue = m.AssetsBaseURL + strings.TrimPrefix(urlValue, "https://assets.grok.com")
		origin = m.AssetsBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlValue, nil)
	if err != nil {
		return nil, "", err
	}
	applyMediaHeaders(req, account, origin, origin+"/")
	response, err := m.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", readHTTPError(response)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("asset download returned empty content")
	}
	mime := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mime == "" {
		mime = mimeFromPath(urlValue)
	}
	return raw, mime, nil
}

func (m *Media) doJSONMap(ctx context.Context, method, endpoint string, account accounts.Account, payload map[string]any) (map[string]any, error) {
	response, err := m.doJSON(ctx, method, endpoint, account, payload, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return nil, fmt.Errorf("decode upstream JSON: %w", err)
	}
	return value, nil
}

func (m *Media) doJSON(ctx context.Context, method, endpoint string, account accounts.Account, payload map[string]any, stream bool) (*http.Response, error) {
	if m.Proxy != nil {
		ctx = proxyruntime.WithURL(ctx, m.Proxy.Resolve(account.Fields, false))
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	applyMediaHeaders(req, account, "https://grok.com", "https://grok.com/")
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	response, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := readHTTPError(response)
		response.Body.Close()
		return nil, err
	}
	return response, nil
}

func applyMediaHeaders(req *http.Request, account accounts.Account, origin, referer string) {
	cookie := firstString(account.Fields["cookie_header"], "")
	if cookie == "" {
		token := strings.TrimPrefix(account.Token, "sso=")
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136 Safari/537.36")
	req.Header.Set("Cookie", cookie)
}

func readHTTPError(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(raw))
	if message == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", response.StatusCode)
	}
	return &protocol.UpstreamError{Status: response.StatusCode, Message: message, Body: message}
}

func firstString(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
		return text
	}
	return fallback
}

func mimeFromPath(value string) string {
	pathValue, _ := url.Parse(value)
	switch strings.ToLower(filepath.Ext(pathValue.Path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	default:
		return "image/jpeg"
	}
}
