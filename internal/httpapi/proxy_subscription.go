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
	proxySubscriptionTimeout  = 20 * time.Second
)

var supportedProxySchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"socks4": true,
	"socks5": true,
}

var proxySubscriptionTokenPattern = regexp.MustCompile(`[\r\n,;\t ]+`)

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

func parseProxySubscription(text string) []string {
	parse := func(input string) []string {
		seen := make(map[string]struct{})
		proxies := make([]string, 0)
		for _, item := range proxySubscriptionTokenPattern.Split(strings.TrimSpace(input), -1) {
			if proxy := normalizeProxySubscriptionURL(item); proxy != "" {
				if _, ok := seen[proxy]; !ok {
					seen[proxy] = struct{}{}
					proxies = append(proxies, proxy)
				}
			}
		}
		return proxies
	}
	if proxies := parse(text); len(proxies) > 0 {
		return proxies
	}

	compact := strings.Join(strings.Fields(text), "")
	if compact == "" {
		return nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil {
			continue
		}
		if string(decoded) == text {
			continue
		}
		if proxies := parse(string(decoded)); len(proxies) > 0 {
			return proxies
		}
	}
	return nil
}

func fetchProxySubscription(subscriptionURL string) (string, error) {
	parsed, err := url.Parse(subscriptionURL)
	if err != nil || parsed.Hostname() == "" || !map[string]bool{"http": true, "https": true}[strings.ToLower(parsed.Scheme)] {
		return "", errors.New("subscription URL must use http or https")
	}
	request, err := http.NewRequest(http.MethodGet, subscriptionURL, nil)
	if err != nil {
		return "", err
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
		return "", errors.New("subscription response exceeds 5 MiB")
	}
	return string(data), nil
}

func proxySubscriptionNode(proxyURL string, index, concurrency int) map[string]any {
	parsed, _ := url.Parse(proxyURL)
	return map[string]any{
		"id":                      fmt.Sprintf("sub-%x", stableProxySubscriptionID(proxyURL)),
		"name":                    fmt.Sprintf("订阅 %d · %s", index, strings.ToLower(parsed.Scheme)),
		"url":                     proxyURL,
		"enabled":                 true,
		"image_concurrency_limit": concurrency,
		"notes":                   "订阅自动管理",
		"source":                  "subscription",
		"subscription_managed":    true,
	}
}

func stableProxySubscriptionID(proxyURL string) [16]byte {
	digest := sha256.Sum256([]byte(proxyURL))
	var result [16]byte
	copy(result[:], digest[:8])
	return result
}

func (s *Server) refreshProxyGroupSubscription(groupID string) (map[string]any, error) {
	normalized := slugID(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(groupID)), "group:"))
	cfg, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	groups := mapList(cfg["proxy_groups"])
	groupIndex := -1
	for index, item := range groups {
		if stringValue(item["id"]) == normalized {
			groupIndex = index
			break
		}
	}
	if groupIndex < 0 {
		return nil, errors.New("proxy group not found")
	}
	group := cloneMap(groups[groupIndex])
	subscriptionURL := strings.TrimSpace(stringValue(group["subscription_url"]))
	if subscriptionURL == "" {
		return nil, errors.New("proxy subscription URL is required")
	}

	text, err := fetchProxySubscription(subscriptionURL)
	if err != nil {
		return nil, err
	}
	proxies := parseProxySubscription(text)
	if len(proxies) == 0 {
		return nil, errors.New("subscription returned no supported proxies")
	}

	concurrency := intValue(group["subscription_node_image_concurrency_limit"])
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
	groups[groupIndex] = group
	updated, err := s.store.UpdateConfig("proxy_groups", groups)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"group":      group,
		"groups":     mapList(updated["proxy_groups"]),
		"node_count": len(subscriptionNodes),
	}, nil
}
