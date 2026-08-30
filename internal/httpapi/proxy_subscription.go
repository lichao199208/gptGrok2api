package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxProxySubscriptionBytes = 5 * 1024 * 1024
	maxProxySubscriptionNodes = 5000
	proxySubscriptionTimeout  = 20 * time.Second
)

var (
	errProxySubscriptionInvalid = errors.New("invalid proxy subscription")
	errProxyGroupNotFound       = errors.New("proxy group not found")
	supportedProxySchemes       = map[string]bool{
		"http": true, "https": true, "socks4": true, "socks5": true,
	}
	proxySubscriptionTokenPattern = regexp.MustCompile(`[\r\n,;\t ]+`)
)

func normalizeProxySubscriptionURL(value string) string {
	raw := strings.Trim(strings.TrimSpace(value), "\ufeff\"'")
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, ";") || strings.HasPrefix(raw, "//") {
		return ""
	}
	if comment := strings.IndexByte(raw, '#'); comment >= 0 {
		raw = strings.TrimSpace(raw[:comment])
	}
	if strings.HasPrefix(strings.ToLower(raw), "socks5h://") {
		raw = "socks5://" + raw[len("socks5h://"):]
	}
	if !strings.Contains(raw, "://") {
		parts := strings.Split(raw, ":")
		if len(parts) == 4 {
			if _, err := strconv.Atoi(parts[1]); err == nil {
				raw = fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
			}
		} else {
			raw = "http://" + raw
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return ""
	}
	if !supportedProxySchemes[strings.ToLower(parsed.Scheme)] {
		return ""
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	return raw
}

func parseProxySubscriptionWithLimit(text string, limit int) ([]string, bool) {
	parse := func(input string) ([]string, bool) {
		seen := make(map[string]struct{})
		proxies := make([]string, 0)
		for _, item := range proxySubscriptionTokenPattern.Split(strings.TrimSpace(input), -1) {
			proxy := normalizeProxySubscriptionURL(item)
			if proxy == "" {
				continue
			}
			if _, ok := seen[proxy]; ok {
				continue
			}
			if limit > 0 && len(proxies) >= limit {
				return nil, true
			}
			seen[proxy] = struct{}{}
			proxies = append(proxies, proxy)
		}
		return proxies, false
	}
	if proxies, overflow := parse(text); overflow || len(proxies) > 0 {
		return proxies, overflow
	}

	compact := strings.Join(strings.Fields(text), "")
	if compact == "" {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil || string(decoded) == text {
			continue
		}
		if proxies, overflow := parse(string(decoded)); overflow || len(proxies) > 0 {
			return proxies, overflow
		}
	}
	return nil, false
}

func parseProxySubscription(text string) []string {
	proxies, _ := parseProxySubscriptionWithLimit(text, 0)
	return proxies
}

func fetchProxySubscription(subscriptionURL string) (string, error) {
	parsed, err := url.Parse(subscriptionURL)
	if err != nil || parsed.Hostname() == "" || !map[string]bool{"http": true, "https": true}[strings.ToLower(parsed.Scheme)] {
		return "", fmt.Errorf("%w: subscription URL must use http or https", errProxySubscriptionInvalid)
	}
	request, err := http.NewRequest(http.MethodGet, subscriptionURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errProxySubscriptionInvalid, err)
	}
	request.Header.Set("User-Agent", "GPTGrok2API-ProxySubscription/1.0")
	client := &http.Client{Timeout: proxySubscriptionTimeout}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("subscription request returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxProxySubscriptionBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxProxySubscriptionBytes {
		return "", fmt.Errorf("%w: subscription response exceeds 5 MiB", errProxySubscriptionInvalid)
	}
	return string(data), nil
}

func proxySubscriptionNode(proxyURL string, index, concurrency int) map[string]any {
	parsed, _ := url.Parse(proxyURL)
	digest := sha256.Sum256([]byte(proxyURL))
	return map[string]any{
		"id":                      fmt.Sprintf("sub-%x", digest[:8]),
		"name":                    fmt.Sprintf("订阅 %d · %s", index, strings.ToLower(parsed.Scheme)),
		"url":                     proxyURL,
		"enabled":                 true,
		"image_concurrency_limit": concurrency,
		"notes":                   "订阅自动管理",
		"source":                  "subscription",
		"subscription_managed":    true,
	}
}

func proxySubscriptionGroupIndex(groups []map[string]any, id string) int {
	for index, group := range groups {
		if stringValue(group["id"]) == id {
			return index
		}
	}
	return -1
}

func proxySubscriptionErrorMessage(err error, subscriptionURL string) string {
	message := strings.TrimSpace(err.Error())
	if subscriptionURL != "" {
		message = strings.ReplaceAll(message, subscriptionURL, "[subscription-url]")
	}
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func (s *Server) recordProxySubscriptionFailure(groupID, subscriptionURL string, cause error) {
	cfg, err := s.store.Config()
	if err != nil {
		return
	}
	groups := mapList(cfg["proxy_groups"])
	index := proxySubscriptionGroupIndex(groups, groupID)
	if index < 0 {
		return
	}
	group := cloneMap(groups[index])
	group["subscription_last_attempt_at"] = time.Now().UTC()
	group["subscription_last_error"] = proxySubscriptionErrorMessage(cause, subscriptionURL)
	groups[index] = group
	_, _ = s.store.UpdateConfig("proxy_groups", groups)
}

func (s *Server) refreshProxyGroupSubscription(groupID string) (map[string]any, error) {
	normalized := slugID(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(groupID)), "group:"))
	cfg, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	groups := mapList(cfg["proxy_groups"])
	groupIndex := proxySubscriptionGroupIndex(groups, normalized)
	if groupIndex < 0 {
		return nil, errProxyGroupNotFound
	}
	subscriptionURL := strings.TrimSpace(stringValue(groups[groupIndex]["subscription_url"]))
	if subscriptionURL == "" {
		return nil, fmt.Errorf("%w: proxy subscription URL is required", errProxySubscriptionInvalid)
	}

	text, err := fetchProxySubscription(subscriptionURL)
	if err != nil {
		s.recordProxySubscriptionFailure(normalized, subscriptionURL, err)
		return nil, err
	}
	proxies, overflow := parseProxySubscriptionWithLimit(text, maxProxySubscriptionNodes)
	if overflow {
		err := fmt.Errorf("%w: subscription contains more than %d proxies", errProxySubscriptionInvalid, maxProxySubscriptionNodes)
		s.recordProxySubscriptionFailure(normalized, subscriptionURL, err)
		return nil, err
	}
	if len(proxies) == 0 {
		err := fmt.Errorf("%w: subscription returned no supported proxies", errProxySubscriptionInvalid)
		s.recordProxySubscriptionFailure(normalized, subscriptionURL, err)
		return nil, err
	}

	// Reload after the network request so a concurrent admin edit is retained.
	latestConfig, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	latestGroups := mapList(latestConfig["proxy_groups"])
	groupIndex = proxySubscriptionGroupIndex(latestGroups, normalized)
	if groupIndex < 0 {
		return nil, errProxyGroupNotFound
	}
	group := cloneMap(latestGroups[groupIndex])
	if latestURL := strings.TrimSpace(stringValue(group["subscription_url"])); latestURL != subscriptionURL {
		return nil, fmt.Errorf("%w: subscription URL changed during refresh", errProxySubscriptionInvalid)
	}
	concurrency := 30
	if raw, exists := group["subscription_node_image_concurrency_limit"]; exists {
		concurrency = intValue(raw)
	}
	if concurrency < 0 || concurrency > 10000 {
		concurrency = 30
	}
	manualNodes := make([]any, 0)
	for _, node := range mapList(group["nodes"]) {
		if boolValue(node["subscription_managed"], false) || strings.EqualFold(stringValue(node["source"]), "subscription") {
			continue
		}
		manualNodes = append(manualNodes, node)
	}
	subscriptionNodes := make([]any, 0, len(proxies))
	for index, proxy := range proxies {
		subscriptionNodes = append(subscriptionNodes, proxySubscriptionNode(proxy, index+1, concurrency))
	}
	group["nodes"] = append(manualNodes, subscriptionNodes...)
	now := time.Now().UTC()
	group["subscription_last_updated_at"] = now
	group["subscription_last_attempt_at"] = now
	group["subscription_last_error"] = ""
	group["subscription_node_count"] = len(subscriptionNodes)
	latestGroups[groupIndex] = group
	updated, err := s.store.UpdateConfig("proxy_groups", latestGroups)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"group":      group,
		"groups":     mapList(updated["proxy_groups"]),
		"node_count": len(subscriptionNodes),
	}, nil
}
