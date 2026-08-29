package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultGPTMailBaseURL = "https://mail.chatgpt.org.uk"

type GPTMail struct {
	HTTP  *http.Client
	mu    sync.Mutex
	cache map[string]gptMailCache
}

type gptMailCache struct {
	ExpiresAt time.Time
	Value     map[string]any
}

func NewGPTMail(client *http.Client) *GPTMail {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GPTMail{HTTP: client, cache: map[string]gptMailCache{}}
}

func (g *GPTMail) Status(ctx context.Context, raw map[string]any, force bool) (map[string]any, error) {
	entry := normalizeGPTMailEntry(raw)
	keyMode := entry["key_mode"].(string)
	base := entry["api_base"].(string)
	apiKey := gptMailString(entry["api_key"])
	cacheKey := base + "|" + keyMode + "|" + apiKey
	if !force {
		g.mu.Lock()
		cached, ok := g.cache[cacheKey]
		g.mu.Unlock()
		if ok && time.Now().Before(cached.ExpiresAt) {
			return cloneAnyMap(cached.Value), nil
		}
	}
	var result map[string]any
	var err error
	if keyMode == "public" {
		result, err = g.publicStatus(ctx, base, true)
	} else {
		if apiKey == "" {
			return nil, fmt.Errorf("GPTMail 自定义模式需要配置 API Key")
		}
		result, err = g.customStatus(ctx, base, apiKey)
	}
	if err != nil {
		return nil, err
	}
	result["key_mode"] = keyMode
	result["api_base"] = base
	keyForHint := gptMailString(result["api_key"])
	if keyForHint == "" {
		keyForHint = apiKey
	}
	result["key_hint"] = gptMailMaskKey(keyForHint)
	delete(result, "api_key")
	result["local_compose"] = gptMailBool(entry["local_compose"])
	result["default_domain"] = gptMailString(entry["default_domain"])
	ttl := 30 * time.Second
	if keyMode == "public" {
		ttl = 60 * time.Second
	}
	g.mu.Lock()
	g.cache[cacheKey] = gptMailCache{ExpiresAt: time.Now().Add(ttl), Value: cloneAnyMap(result)}
	g.mu.Unlock()
	return result, nil
}

func (g *GPTMail) RefreshPublicKey(ctx context.Context, raw map[string]any, force bool) (map[string]any, error) {
	entry := normalizeGPTMailEntry(raw)
	if entry["key_mode"] != "public" {
		return nil, fmt.Errorf("只有 GPTMail 公共 Key 模式需要自动刷新 Key")
	}
	result, err := g.publicStatus(ctx, entry["api_base"].(string), force)
	if err != nil {
		return nil, err
	}
	key := gptMailString(result["api_key"])
	if key == "" {
		return nil, fmt.Errorf("GPTMail 公共 Key 获取失败")
	}
	return map[string]any{"ok": true, "key_mode": "public", "api_base": entry["api_base"], "source": result["source"], "is_active": result["is_active"], "daily_limit": result["daily_limit"], "used_today": result["used_today"], "remaining_today": result["remaining_today"], "reset_at": result["reset_at"], "seconds_until_reset": result["seconds_until_reset"], "key_hint": gptMailMaskKey(key), "local_compose": gptMailBool(entry["local_compose"]), "default_domain": gptMailString(entry["default_domain"]), "refreshed_at": time.Now().UTC().Format(time.RFC3339)}, nil
}

func (g *GPTMail) publicStatus(ctx context.Context, base string, reveal bool) (map[string]any, error) {
	query := url.Values{}
	if reveal {
		query.Set("reveal", "1")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/public-key-status?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Mozilla/5.0")
	if reveal {
		request.Header.Set("X-Public-Key-Reveal", "click")
	}
	return g.doStatus(request, "GPTMail 公共 Key 状态请求")
}

func (g *GPTMail) customStatus(ctx context.Context, base, apiKey string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/stats", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Mozilla/5.0")
	request.Header.Set("X-API-Key", apiKey)
	result, err := g.doStatus(request, "GPTMail 自定义 Key 状态请求")
	if err != nil {
		return nil, err
	}
	usage := mapValue(result["usage"])
	for _, key := range []string{"daily_limit", "used_today", "remaining_today", "total_limit", "total_usage", "remaining_total", "reset_at", "seconds_until_reset"} {
		if value, ok := usage[key]; ok {
			result[key] = value
		}
	}
	result["source"] = "stats"
	result["is_active"] = true
	return result, nil
}

func (g *GPTMail) doStatus(request *http.Request, label string) (map[string]any, error) {
	response, err := g.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s失败: %w", label, err)
	}
	defer response.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s失败: HTTP %d", label, response.StatusCode)
	}
	if success, ok := payload["success"].(bool); ok && !success {
		return nil, fmt.Errorf("%s返回异常: %s", label, gptMailString(payload["error"]))
	}
	if data, ok := payload["data"].(map[string]any); ok {
		for key, value := range data {
			payload[key] = value
		}
	}
	if key := gptMailString(payload["key"]); key != "" && gptMailString(payload["api_key"]) == "" {
		payload["api_key"] = key
	}
	return payload, nil
}

func normalizeGPTMailEntry(raw map[string]any) map[string]any {
	result := cloneAnyMap(raw)
	base := strings.TrimRight(strings.TrimSpace(gptMailString(result["api_base"])), "/")
	if base == "" {
		base = DefaultGPTMailBaseURL
	}
	mode := strings.ToLower(strings.TrimSpace(gptMailString(result["key_mode"])))
	if mode != "public" && mode != "custom" {
		if gptMailString(result["api_key"]) == "" {
			mode = "public"
		} else {
			mode = "custom"
		}
	}
	result["api_base"] = base
	result["key_mode"] = mode
	return result
}
func cloneAnyMap(input map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range input {
		result[key] = value
	}
	return result
}
func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}
func gptMailString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
func gptMailBool(value any) bool { typed, ok := value.(bool); return ok && typed }
func gptMailMaskKey(value string) string {
	if len(value) <= 8 {
		if value == "" {
			return ""
		}
		return strings.Repeat("*", len(value))
	}
	return value[:5] + "..." + value[len(value)-4:]
}
