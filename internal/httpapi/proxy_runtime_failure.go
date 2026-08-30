package httpapi

import (
	"log"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func (s *Server) persistProxyGroupRuntimeResult(event proxyruntime.ImageNodeRuntimeResult) {
	if s == nil || s.store == nil || event.GroupID == "" || event.NodeID == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.store.MutateConfig("proxy_groups", func(value any) (any, error) {
		groups := mapList(value)
		for groupIndex, group := range groups {
			if stringValue(group["id"]) != event.GroupID {
				continue
			}
			nextGroup := cloneMap(group)
			nodes := mapList(group["nodes"])
			nextNodes := make([]map[string]any, 0, len(nodes))
			found := false
			for _, node := range nodes {
				if stringValue(node["id"]) != event.NodeID {
					nextNodes = append(nextNodes, node)
					continue
				}
				found = true
				if event.Removed {
					continue
				}
				nextNode := cloneMap(node)
				nextNode["runtime_failure_count"] = event.Failures
				nextNode["runtime_success_count"] = event.Successes
				if event.Failures > 0 {
					nextNode["runtime_last_failure_at"] = now
				} else {
					nextNode["runtime_last_failure_at"] = ""
				}
				nextNodes = append(nextNodes, nextNode)
			}
			if !found {
				return groups, nil
			}
			nextGroup["nodes"] = nextNodes
			if event.Removed {
				removedURLs := stringList(group["runtime_removed_proxy_urls"])
				normalized := normalizeSubscriptionProxyURL(event.URL)
				if normalized != "" && !containsString(removedURLs, normalized) {
					removedURLs = append(removedURLs, normalized)
				}
				nextGroup["runtime_removed_proxy_urls"] = removedURLs
				nextGroup["runtime_last_removed_at"] = now
				nextGroup["runtime_last_removed_node_id"] = event.NodeID
				nextGroup["runtime_removed_count"] = intValue(group["runtime_removed_count"]) + 1
				subscriptionCount := 0
				for _, node := range nextNodes {
					if isSubscriptionProxyNode(node) {
						subscriptionCount++
					}
				}
				nextGroup["subscription_node_count"] = subscriptionCount
			}
			groups[groupIndex] = nextGroup
			return groups, nil
		}
		return groups, nil
	})
	if err != nil {
		log.Printf("persist proxy node runtime result failed: %v", err)
		return
	}
	if event.Removed {
		log.Printf("removed proxy group node after %d runtime failures: group=%s node=%s", event.Failures, event.GroupID, event.NodeID)
	}
}
