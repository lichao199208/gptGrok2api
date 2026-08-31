package httpapi

import (
	"context"
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
	proxyGroupTestConcurrency = 32
	proxyGroupTestTimeout     = 5 * time.Second
	defaultProxyProbeURL      = "https://chatgpt.com/api/auth/csrf"
)

type proxyGroupNodeTest struct {
	NodeID string         `json:"node_id"`
	Result map[string]any `json:"result"`
}

func (s *Server) proxyGroupTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		ID          string `json:"id"`
		NodeID      string `json:"node_id"`
		PruneFailed bool   `json:"prune_failed"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	groupID := slugID(request.ID)
	nodeID := slugID(request.NodeID)
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "proxy group id is required", "invalid_request_error")
		return
	}

	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	var group map[string]any
	for _, candidate := range mapList(cfg["proxy_groups"]) {
		if slugID(stringValue(candidate["id"])) == groupID {
			group = candidate
			break
		}
	}
	if group == nil {
		writeError(w, http.StatusNotFound, "proxy group not found", "not_found")
		return
	}
	nodes := mapList(group["nodes"])
	if nodeID != "" {
		selected := make([]map[string]any, 0, 1)
		for _, node := range nodes {
			if slugID(stringValue(node["id"])) == nodeID {
				selected = append(selected, node)
				break
			}
		}
		if len(selected) == 0 {
			writeError(w, http.StatusNotFound, "proxy node not found", "not_found")
			return
		}
		nodes = selected
	}
	if len(nodes) == 0 {
		writeError(w, http.StatusBadRequest, "proxy group has no nodes", "invalid_request_error")
		return
	}

	results := s.testProxyGroupNodes(r.Context(), nodes)
	updated, err := s.persistProxyGroupHealth(groupID, results, request.PruneFailed && nodeID == "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if err := s.refreshProxyRuntime(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	response := map[string]any{"results": results, "groups": mapList(updated["proxy_groups"])}
	if nodeID != "" && len(results) == 1 {
		response["result"] = results[0].Result
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) testProxyGroupNodes(ctx context.Context, nodes []map[string]any) []proxyGroupNodeTest {
	results := make([]proxyGroupNodeTest, len(nodes))
	client := &http.Client{
		Transport: proxyruntime.NewTransport(http.DefaultTransport),
		Timeout:   proxyGroupTestTimeout,
	}
	semaphore := make(chan struct{}, proxyGroupTestConcurrency)
	var wg sync.WaitGroup
	for index, node := range nodes {
		index, node := index, node
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index] = proxyGroupNodeTest{NodeID: stringValue(node["id"]), Result: failedProxyTestResult(ctx.Err())}
				return
			}
			results[index] = proxyGroupNodeTest{
				NodeID: stringValue(node["id"]),
				Result: s.testProxyGroupCandidate(ctx, client, stringValue(node["url"])),
			}
		}()
	}
	wg.Wait()
	return results
}

func (s *Server) testProxyGroupCandidate(parent context.Context, client *http.Client, candidate string) map[string]any {
	started := time.Now()
	result := map[string]any{"ok": false, "reachable": false, "verification": "failed", "status": 0, "latency_ms": int64(0), "has_proxy": true, "proxy_source": "group"}
	parsed, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil || parsed.Host == "" {
		result["error"] = "invalid proxy url"
		return result
	}
	ctx, cancel := context.WithTimeout(parent, proxyGroupTestTimeout)
	defer cancel()
	target := strings.TrimSpace(s.proxyProbeURL)
	if target == "" {
		target = defaultProxyProbeURL
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, candidate), http.MethodGet, target, nil)
	if err == nil {
		var response *http.Response
		response, err = client.Do(req)
		if response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
			_ = response.Body.Close()
			result["status"] = response.StatusCode
			result["ok"] = proxyTestStatusOK(response.StatusCode)
			result["reachable"] = proxyTestReachable(response.StatusCode)
			if response.StatusCode == http.StatusForbidden {
				result["verification"] = "api_required"
				result["status_label"] = "可连接，需真实图片验证"
			} else if proxyTestStatusOK(response.StatusCode) {
				result["verification"] = "probe_ok"
				result["status_label"] = "可用"
			} else {
				result["error"] = fmt.Sprintf("HTTP %d", response.StatusCode)
			}
		}
	}
	result["latency_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		result["ok"] = false
		result["status"] = 0
		result["error"] = redactProxyError(err.Error())
	}
	return result
}

func proxyTestStatusOK(status int) bool {
	return (status >= http.StatusOK && status < http.StatusBadRequest) || status == http.StatusForbidden
}

func proxyTestReachable(status int) bool {
	return status >= http.StatusOK && status < http.StatusInternalServerError
}

func failedProxyTestResult(err error) map[string]any {
	message := "proxy test cancelled"
	if err != nil {
		message = err.Error()
	}
	return map[string]any{"ok": false, "reachable": false, "verification": "failed", "status": 0, "latency_ms": int64(0), "error": message, "has_proxy": true, "proxy_source": "group"}
}

func (s *Server) persistProxyGroupHealth(groupID string, results []proxyGroupNodeTest, pruneFailed bool) (map[string]any, error) {
	byNodeID := make(map[string]map[string]any, len(results))
	for _, item := range results {
		if id := slugID(item.NodeID); id != "" {
			byNodeID[id] = item.Result
		}
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	return s.store.MutateConfig("proxy_groups", func(value any) (any, error) {
		groups := mapList(value)
		found := false
		for groupIndex, currentGroup := range groups {
			if slugID(stringValue(currentGroup["id"])) != groupID {
				continue
			}
			found = true
			nextGroup := cloneMap(currentGroup)
			nodes := mapList(currentGroup["nodes"])
			failed := make(map[string]bool)
			if pruneFailed {
				for _, item := range results {
					if !boolValue(item.Result["ok"], false) {
						failed[slugID(item.NodeID)] = true
					}
				}
			}
			kept := make([]map[string]any, 0, len(nodes))
			for _, currentNode := range nodes {
				if pruneFailed && boolValue(currentNode["enabled"], true) && failed[slugID(stringValue(currentNode["id"]))] {
					continue
				}
				result := byNodeID[slugID(stringValue(currentNode["id"]))]
				if result == nil {
					kept = append(kept, currentNode)
					continue
				}
				nextNode := cloneMap(currentNode)
				nextNode["last_checked_at"] = checkedAt
				nextNode["last_latency_ms"] = intValue(result["latency_ms"])
				nextNode["last_status"] = intValue(result["status"])
				nextNode["last_verification"] = stringValue(result["verification"])
				nextNode["last_status_label"] = stringValue(result["status_label"])
				if boolValue(result["ok"], false) {
					nextNode["last_error"] = ""
					nextNode["last_error_at"] = ""
				} else {
					nextNode["last_error"] = firstNonEmpty(stringValue(result["error"]), "检测失败")
					nextNode["last_error_at"] = checkedAt
				}
				kept = append(kept, nextNode)
			}
			nextGroup["nodes"] = kept
			groups[groupIndex] = nextGroup
			break
		}
		if !found {
			return nil, fmt.Errorf("proxy group not found")
		}
		return groups, nil
	})
}
