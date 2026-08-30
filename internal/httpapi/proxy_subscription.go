package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxProxySubscriptionBytes = 8 << 20
	maxProxySubscriptionNodes = 5000
)

var (
	errProxyGroupNotFound          = errors.New("proxy group not found")
	errProxySubscriptionURLChanged = errors.New("proxy subscription URL changed while refresh was running")
)

func (s *Server) refreshProxyGroupSubscription(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id = strings.TrimSpace(id)
	group, err := s.proxyGroupConfig(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}
	subscriptionURL := strings.TrimSpace(stringValue(group["subscription_url"]))
	parsedURL, err := url.Parse(subscriptionURL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		err = errors.New("proxy subscription URL must use http or https")
		s.recordProxySubscriptionError(id, subscriptionURL, err)
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	nodes, err := s.fetchProxySubscription(r, parsedURL.String())
	if err != nil {
		s.recordProxySubscriptionError(id, subscriptionURL, err)
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	updatedGroup, groups, err := s.applyProxySubscription(id, subscriptionURL, nodes)
	if err != nil {
		status := http.StatusInternalServerError
		errorType := "server_error"
		if errors.Is(err, errProxyGroupNotFound) {
			status, errorType = http.StatusNotFound, "not_found"
		} else if errors.Is(err, errProxySubscriptionURLChanged) {
			status, errorType = http.StatusConflict, "conflict_error"
		}
		writeError(w, status, err.Error(), errorType)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "group": updatedGroup, "groups": groups,
		"node_count": intValue(updatedGroup["subscription_node_count"]), "refreshed_at": updatedGroup["subscription_last_updated_at"],
	})
}

func (s *Server) proxyGroupConfig(id string) (map[string]any, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	for _, group := range mapList(cfg["proxy_groups"]) {
		if stringValue(group["id"]) == id {
			return group, nil
		}
	}
	return nil, errProxyGroupNotFound
}

func (s *Server) fetchProxySubscription(r *http.Request, subscriptionURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, subscriptionURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, application/octet-stream;q=0.9, */*;q=0.8")
	req.Header.Set("User-Agent", "gptgrok2api-go/1.2 proxy-subscription")
	response, err := s.requestClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request proxy subscription: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy subscription returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxProxySubscriptionBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read proxy subscription: %w", err)
	}
	if len(raw) > maxProxySubscriptionBytes {
		return nil, fmt.Errorf("proxy subscription exceeds %d MiB", maxProxySubscriptionBytes>>20)
	}
	return parseProxySubscription(raw)
}

func parseProxySubscription(raw []byte) ([]string, error) {
	if nodes, tooMany := proxyURLsFromText(string(raw)); len(nodes) > 0 || tooMany {
		if tooMany {
			return nil, fmt.Errorf("proxy subscription exceeds %d nodes", maxProxySubscriptionNodes)
		}
		return nodes, nil
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, string(raw))
	for _, decoder := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := decoder.DecodeString(compact)
		if err != nil {
			continue
		}
		nodes, tooMany := proxyURLsFromText(string(decoded))
		if tooMany {
			return nil, fmt.Errorf("proxy subscription exceeds %d nodes", maxProxySubscriptionNodes)
		}
		if len(nodes) > 0 {
			return nodes, nil
		}
	}
	return nil, errors.New("proxy subscription contains no supported proxy nodes")
}

func proxyURLsFromText(value string) ([]string, bool) {
	seen := map[string]bool{}
	result := make([]string, 0)
	for _, line := range strings.Split(strings.TrimPrefix(value, "\ufeff"), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.Replace(line, `\://`, "://", 1)
		normalized := normalizeSubscriptionProxyURL(line)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, normalized)
		if len(result) > maxProxySubscriptionNodes {
			return nil, true
		}
	}
	return result, false
}

func normalizeSubscriptionProxyURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return ""
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return ""
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return ""
	}
	parsed.Host = net.JoinHostPort(strings.ToLower(host), port)
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	return parsed.String()
}

func proxySubscriptionNode(proxyURL string, index, concurrency int) map[string]any {
	digest := sha256.Sum256([]byte(proxyURL))
	return map[string]any{
		"id": fmt.Sprintf("sub-%x", digest[:8]), "name": fmt.Sprintf("订阅 %d", index+1),
		"url": proxyURL, "enabled": true, "image_concurrency_limit": concurrency,
		"source": "subscription", "subscription_managed": true,
	}
}

func isSubscriptionProxyNode(node map[string]any) bool {
	return boolValue(node["subscription_managed"], false) || strings.EqualFold(stringValue(node["source"]), "subscription")
}

func (s *Server) applyProxySubscription(id, subscriptionURL string, proxyURLs []string) (map[string]any, []map[string]any, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var updatedGroup map[string]any
	updated, err := s.store.MutateConfig("proxy_groups", func(value any) (any, error) {
		groups := mapList(value)
		found := false
		for index, current := range groups {
			if stringValue(current["id"]) != id {
				continue
			}
			found = true
			if strings.TrimSpace(stringValue(current["subscription_url"])) != subscriptionURL {
				return nil, errProxySubscriptionURLChanged
			}
			next := cloneMap(current)
			manualNodes := make([]map[string]any, 0)
			seen := map[string]bool{}
			blocked := map[string]bool{}
			for _, blockedURL := range stringList(current["runtime_removed_proxy_urls"]) {
				if normalized := normalizeSubscriptionProxyURL(blockedURL); normalized != "" {
					blocked[normalized] = true
				}
			}
			for _, node := range mapList(current["nodes"]) {
				if isSubscriptionProxyNode(node) {
					continue
				}
				manualNodes = append(manualNodes, node)
				if normalized := normalizeSubscriptionProxyURL(stringValue(node["url"])); normalized != "" {
					seen[normalized] = true
				}
			}
			concurrency := intValue(current["subscription_node_image_concurrency_limit"])
			subscriptionCount := 0
			for _, proxyURL := range proxyURLs {
				if seen[proxyURL] || blocked[proxyURL] {
					continue
				}
				seen[proxyURL] = true
				manualNodes = append(manualNodes, proxySubscriptionNode(proxyURL, subscriptionCount, concurrency))
				subscriptionCount++
			}
			next["nodes"] = manualNodes
			next["subscription_last_attempt_at"] = now
			next["subscription_last_updated_at"] = now
			next["subscription_last_error"] = ""
			next["subscription_node_count"] = subscriptionCount
			groups[index] = next
			updatedGroup = next
			break
		}
		if !found {
			return nil, errProxyGroupNotFound
		}
		return groups, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return updatedGroup, mapList(updated["proxy_groups"]), nil
}

func (s *Server) recordProxySubscriptionError(id, subscriptionURL string, refreshErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.store.MutateConfig("proxy_groups", func(value any) (any, error) {
		groups := mapList(value)
		for index, current := range groups {
			if stringValue(current["id"]) != id || strings.TrimSpace(stringValue(current["subscription_url"])) != subscriptionURL {
				continue
			}
			next := cloneMap(current)
			next["subscription_last_attempt_at"] = now
			next["subscription_last_error"] = refreshErr.Error()
			groups[index] = next
			break
		}
		return groups, nil
	})
}
