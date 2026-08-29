package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type runtimeMonitor struct {
	mu        sync.Mutex
	active    map[string]*monitorRecord
	completed []monitorRecord
	limit     int
}

type monitorRecord struct {
	CallID       string           `json:"call_id"`
	Endpoint     string           `json:"endpoint"`
	Model        string           `json:"model"`
	Summary      string           `json:"summary,omitempty"`
	Status       string           `json:"status"`
	Stage        string           `json:"stage"`
	StartedAt    int64            `json:"started_ts"`
	UpdatedAt    int64            `json:"updated_ts"`
	EndedAt      int64            `json:"ended_ts,omitempty"`
	Duration     int64            `json:"duration_ms,omitempty"`
	Progress     int              `json:"progress,omitempty"`
	Error        string           `json:"error,omitempty"`
	Metrics      map[string]any   `json:"metrics,omitempty"`
	Perf         map[string]any   `json:"perf,omitempty"`
	Events       []map[string]any `json:"events,omitempty"`
	RequestMeta  map[string]any   `json:"request_meta,omitempty"`
	AccountEmail string           `json:"account_email,omitempty"`
	AccountID    string           `json:"account_id,omitempty"`
	KeyName      string           `json:"key_name,omitempty"`
	KeyID        string           `json:"key_id,omitempty"`
	ProxySource  string           `json:"proxy_source,omitempty"`
	EgressMode   string           `json:"egress_mode,omitempty"`
	EgressLabel  string           `json:"egress_label,omitempty"`
	HasProxy     bool             `json:"has_proxy"`
}

func newRuntimeMonitor() *runtimeMonitor {
	return &runtimeMonitor{active: map[string]*monitorRecord{}, limit: 200}
}

func (m *runtimeMonitor) start(id, endpoint, model, summary string) {
	if m == nil || strings.TrimSpace(id) == "" {
		return
	}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	m.active[id] = &monitorRecord{CallID: id, Endpoint: endpoint, Model: model, Summary: summary, Status: "running", Stage: "handler_submitted", StartedAt: now, UpdatedAt: now, Events: []map[string]any{{"event": "handler_submitted", "label": monitorStageLabel("handler_submitted"), "stage": "handler_submitted", "status": "running", "time": time.Now().UTC()}}}
	m.mu.Unlock()
}

func (m *runtimeMonitor) update(id, stage string, progress int, errText string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.active[id]
	if item == nil {
		return
	}
	item.Stage = stage
	item.UpdatedAt = time.Now().UnixMilli()
	if progress >= 0 {
		item.Progress = progress
	}
	if errText != "" {
		item.Error = errText
	}
	item.Events = append(item.Events, map[string]any{"event": stage, "label": monitorStageLabel(stage), "stage": stage, "progress": item.Progress, "status": item.Status, "time": time.Now().UTC()})
}

func (m *runtimeMonitor) enrich(id string, body map[string]any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.active[id]
	if item == nil {
		return
	}
	if v, ok := body["metrics"].(map[string]any); ok {
		if item.Metrics == nil {
			item.Metrics = map[string]any{}
		}
		for k, x := range v {
			item.Metrics[k] = x
		}
	}
	if v, ok := body["perf"].(map[string]any); ok {
		if item.Perf == nil {
			item.Perf = map[string]any{}
		}
		for k, x := range v {
			item.Perf[k] = x
		}
	}
	for k, v := range body {
		if strings.HasSuffix(k, "_ms") {
			if item.Metrics == nil {
				item.Metrics = map[string]any{}
			}
			item.Metrics[k] = v
		}
	}
	if value := stringValue(body["proxy_source"]); value != "" {
		item.ProxySource = value
	}
	if value := stringValue(body["egress_mode"]); value != "" {
		item.EgressMode = value
	}
	if value := stringValue(body["egress_label"]); value != "" {
		item.EgressLabel = value
	}
	if value, ok := body["has_proxy"].(bool); ok {
		item.HasProxy = value
	}
	if value := stringValue(body["account_email"]); value != "" {
		item.AccountEmail = value
	}
	if value := stringValue(body["account_id"]); value != "" {
		item.AccountID = value
	}
	if value := stringValue(body["key_name"]); value != "" {
		item.KeyName = value
	}
	if value := stringValue(body["key_id"]); value != "" {
		item.KeyID = value
	}
	if len(item.Events) > 0 {
		for k, v := range body {
			if k != "call_id" {
				item.Events[len(item.Events)-1][k] = v
			}
		}
	}
}

func (m *runtimeMonitor) finish(id, status, model, summary, errText string) {
	if m == nil {
		return
	}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	item := m.active[id]
	if item == nil {
		item = &monitorRecord{CallID: id, StartedAt: now}
	}
	item.Status = status
	item.Stage = map[string]string{"success": "completed", "failed": "failed", "cancelled": "cancelled"}[status]
	if item.Stage == "" {
		item.Stage = status
	}
	item.Model = firstNonEmpty(model, item.Model)
	item.Summary = firstNonEmpty(summary, item.Summary)
	item.Error = firstNonEmpty(errText, item.Error)
	item.EndedAt = now
	item.UpdatedAt = now
	item.Duration = now - item.StartedAt
	if status == "success" {
		item.Progress = 100
	}
	item.Events = append(item.Events, map[string]any{"event": "completed", "label": monitorStageLabel(item.Stage), "stage": item.Stage, "status": status, "duration_ms": item.Duration, "time": time.Now().UTC()})
	copy := *item
	delete(m.active, id)
	m.completed = append(m.completed, copy)
	if len(m.completed) > m.limit {
		m.completed = m.completed[len(m.completed)-m.limit:]
	}
	m.mu.Unlock()
}

func (m *runtimeMonitor) snapshot() map[string]any {
	if m == nil {
		return map[string]any{"active": []monitorRecord{}, "recent": []monitorRecord{}}
	}
	m.mu.Lock()
	active := make([]monitorRecord, 0, len(m.active))
	for _, item := range m.active {
		copy := *item
		copy.Duration = time.Now().UnixMilli() - copy.StartedAt
		active = append(active, copy)
	}
	recent := append([]monitorRecord(nil), m.completed...)
	m.mu.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i].StartedAt < active[j].StartedAt })
	sort.Slice(recent, func(i, j int) bool { return recent[i].EndedAt > recent[j].EndedAt })
	return map[string]any{
		"updated_at": time.Now().UTC(),
		"summary":    map[string]any{"active": len(active), "completed": len(recent), "failed": countMonitorStatus(recent, "failed")},
		"active":     active,
		"recent":     recent,
		"slow":       recent,
	}
}

func (m *runtimeMonitor) detail(id string) (monitorRecord, bool) {
	if m == nil {
		return monitorRecord{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if item := m.active[id]; item != nil {
		copy := *item
		copy.Duration = time.Now().UnixMilli() - copy.StartedAt
		return copy, true
	}
	for index := len(m.completed) - 1; index >= 0; index-- {
		if m.completed[index].CallID == id {
			return m.completed[index], true
		}
	}
	return monitorRecord{}, false
}

func (m *runtimeMonitor) cancel(id string) (monitorRecord, bool) {
	item, ok := m.detail(id)
	if !ok || item.Status != "running" {
		return monitorRecord{}, false
	}
	m.finish(id, "cancelled", item.Model, item.Summary, "cancelled by administrator")
	item, _ = m.detail(id)
	return item, true
}

func countMonitorStatus(items []monitorRecord, status string) int {
	count := 0
	for _, item := range items {
		if item.Status == status {
			count++
		}
	}
	return count
}

func (s *Server) proxyTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		URL string `json:"url"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &request) {
		return
	}
	candidate := strings.TrimSpace(request.URL)
	if candidate == "" {
		candidate = s.proxyManager.Resolve(nil, false)
	}
	if candidate == "" {
		writeError(w, http.StatusBadRequest, "proxy url is required", "invalid_request_error")
		return
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": false, "status": 0, "latency_ms": 0, "error": "invalid proxy url", "has_proxy": true}})
		return
	}
	client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport), Timeout: 15 * time.Second}
	started := time.Now()
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(r.Context(), candidate), http.MethodGet, "https://chatgpt.com/api/auth/csrf", nil)
	result := map[string]any{"proxy_source": "input", "has_proxy": true}
	if err == nil {
		var response *http.Response
		response, err = client.Do(req)
		if response != nil {
			defer response.Body.Close()
			result["status"] = response.StatusCode
			result["ok"] = response.StatusCode < 500
			if response.StatusCode >= 500 {
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
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func redactProxyError(value string) string {
	for _, scheme := range []string{"http://", "https://", "socks5://", "socks5h://", "socks://"} {
		for {
			start := strings.Index(strings.ToLower(value), scheme)
			if start < 0 {
				break
			}
			at := strings.Index(value[start:], "@")
			if at < 0 {
				break
			}
			value = value[:start+len(scheme)] + "[REDACTED]@" + value[start+at+1:]
		}
	}
	return value
}

func (s *Server) monitorAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/monitor/realtime")
	if path == "" || path == "/" {
		writeJSON(w, http.StatusOK, s.monitorSnapshotWithHistory())
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	itemID := parts[0]
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		item, ok := s.monitor.cancel(itemID)
		if !ok {
			writeError(w, http.StatusNotFound, "request not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": item})
		return
	}
	item, ok := s.monitor.detail(itemID)
	if !ok {
		writeError(w, http.StatusNotFound, "request not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"detail": item})
}

func (s *Server) monitorSnapshotWithHistory() map[string]any {
	snapshot := s.monitor.snapshot()
	active := monitorRecordMaps(snapshot["active"])
	recent := monitorRecordMaps(snapshot["recent"])
	history, events := s.historicalMonitorRecords(200)

	seen := map[string]bool{}
	for _, item := range recent {
		seen[stringValue(item["call_id"])] = true
	}
	for _, item := range history {
		id := stringValue(item["call_id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		recent = append(recent, item)
	}
	sort.SliceStable(recent, func(i, j int) bool {
		return stringValue(recent[i]["ended_at"]) > stringValue(recent[j]["ended_at"])
	})
	if len(recent) > 200 {
		recent = recent[:200]
	}

	slow := append([]map[string]any(nil), recent...)
	sort.SliceStable(slow, func(i, j int) bool {
		return monitorNumber(slow[i]["duration_ms"]) > monitorNumber(slow[j]["duration_ms"])
	})
	if len(slow) > 50 {
		slow = slow[:50]
	}

	recentCounts := monitorCountsByField(recent, func(item map[string]any) string {
		return stringValue(item["model"])
	})
	activeCounts := monitorCountsByField(active, func(item map[string]any) string {
		return stringValue(item["model"])
	})
	if len(activeCounts) == 0 {
		activeCounts = recentCounts
	}

	activeStageCounts := monitorCountsByField(active, func(item map[string]any) string {
		stage := strings.TrimSpace(stringValue(item["stage"]))
		if stage == "" {
			stage = strings.TrimSpace(stringValue(item["stage_label"]))
		}
		return stage
	})
	if len(activeStageCounts) == 0 {
		activeStageCounts = monitorCountsByField(recent, func(item map[string]any) string {
			stage := strings.TrimSpace(stringValue(item["stage"]))
			if stage == "" {
				stage = strings.TrimSpace(stringValue(item["stage_label"]))
			}
			return stage
		})
	}

	activeEgressCounts := monitorCountsByField(active, monitorEgressKey)
	if len(activeEgressCounts) == 0 {
		activeEgressCounts = monitorCountsByField(recent, monitorEgressKey)
	}

	durationValues := monitorMetricValues(recent, "duration_ms")
	metricValues := monitorMetricValuesMap(recent, monitorMonitorMetricKeys)
	metricP95 := map[string]any{}
	metricLabels := map[string]string{}
	bottleneckKey := ""
	bottleneckLabel := ""
	var bottleneckValue int64
	for _, key := range monitorMonitorMetricKeys {
		values := metricValues[key]
		if len(values) == 0 {
			continue
		}
		p95 := monitorPercentile(values, 0.95)
		if p95 <= 0 {
			continue
		}
		metricP95[key] = p95
		metricLabels[key] = monitorMetricLabel(key)
		if p95 > bottleneckValue {
			bottleneckValue = p95
			bottleneckKey = key
			bottleneckLabel = monitorMetricLabel(key)
		}
	}

	summary := map[string]any{
		"active":          len(active),
		"completed":       len(recent),
		"success":         0,
		"failed":          0,
		"success_rate":    0,
		"avg_duration_ms": 0,
		"p95_duration_ms": monitorPercentile(durationValues, 0.95),
		"metric_p95":      metricP95,
		"slow_counts": map[string]any{
			"handler_queue": 0, "stream_first_queue": 0, "account_wait": 0,
			"egress_wait": 0, "total_over_120s": 0, "local_reject_or_busy": 0,
		},
		"bottleneck":       map[string]any{"key": bottleneckKey, "label": bottleneckLabel, "value_ms": bottleneckValue},
		"by_model":         recentCounts,
		"active_by_model":  activeCounts,
		"active_by_stage":  activeStageCounts,
		"active_by_egress": activeEgressCounts,
	}
	var totalDuration int64
	for _, item := range recent {
		status := strings.ToLower(stringValue(item["status"]))
		switch status {
		case "success", "completed":
			summary["success"] = monitorNumber(summary["success"]) + 1
		case "failed", "error":
			summary["failed"] = monitorNumber(summary["failed"]) + 1
		}
		duration := monitorNumber(item["duration_ms"])
		totalDuration += duration
		if duration >= 120000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["total_over_120s"] = monitorNumber(slowCounts["total_over_120s"]) + 1
		}
		if monitorNumber(item["handler_queue_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["handler_queue"] = monitorNumber(slowCounts["handler_queue"]) + 1
		}
		if monitorNumber(item["stream_first_queue_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["stream_first_queue"] = monitorNumber(slowCounts["stream_first_queue"]) + 1
		}
		if monitorNumber(item["account_wait_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["account_wait"] = monitorNumber(slowCounts["account_wait"]) + 1
		}
		if monitorNumber(item["egress_wait_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["egress_wait"] = monitorNumber(slowCounts["egress_wait"]) + 1
		}
		if monitorLooksLocallyBusy(item) {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["local_reject_or_busy"] = monitorNumber(slowCounts["local_reject_or_busy"]) + 1
		}
	}
	success := monitorNumber(summary["success"])
	if len(recent) > 0 {
		summary["avg_duration_ms"] = totalDuration / int64(len(recent))
		summary["success_rate"] = float64(success) * 100 / float64(len(recent))
	}
	snapshot["active"] = active
	snapshot["recent"] = recent
	snapshot["slow"] = slow
	snapshot["events"] = events
	snapshot["summary"] = summary
	snapshot["threadpool"] = map[string]any{"tokens": 0, "previous_tokens": 0}
	snapshot["window"] = map[string]any{"completed": len(recent), "completed_capacity": 200, "events": len(events), "event_capacity": 1000}
	snapshot["metric_labels"] = metricLabels
	return snapshot
}

func monitorRecordMaps(value any) []map[string]any {
	result := []map[string]any{}
	switch typed := value.(type) {
	case []monitorRecord:
		for _, item := range typed {
			result = append(result, monitorRecordMap(item))
		}
	case []map[string]any:
		result = append(result, typed...)
	case []any:
		for _, item := range typed {
			if record, ok := item.(map[string]any); ok {
				result = append(result, record)
			}
		}
	}
	return result
}

func monitorRecordMap(item monitorRecord) map[string]any {
	modelName := item.Model
	if modelName == "" && item.RequestMeta != nil {
		modelName = stringValue(item.RequestMeta["model"])
	}
	result := map[string]any{
		"call_id": item.CallID, "endpoint": item.Endpoint, "model": modelName,
		"summary": item.Summary, "status": item.Status, "stage": item.Stage,
		"duration_ms": item.Duration, "elapsed_ms": item.Duration, "progress": item.Progress,
	}
	result["stage_label"] = monitorStageLabel(item.Stage)
	if item.StartedAt > 0 {
		result["started_at"] = time.UnixMilli(item.StartedAt).UTC().Format(time.RFC3339)
	}
	if item.UpdatedAt > 0 {
		result["updated_at"] = time.UnixMilli(item.UpdatedAt).UTC().Format(time.RFC3339)
	}
	if item.EndedAt > 0 {
		result["ended_at"] = time.UnixMilli(item.EndedAt).UTC().Format(time.RFC3339)
	}
	if item.Error != "" {
		result["error"] = item.Error
	}
	result["metrics"] = item.Metrics
	result["perf"] = item.Perf
	result["events"] = item.Events
	result["request_meta"] = item.RequestMeta
	result["account_email"] = item.AccountEmail
	result["account_id"] = item.AccountID
	result["key_name"] = item.KeyName
	result["key_id"] = item.KeyID
	result["proxy_source"] = item.ProxySource
	result["egress_mode"] = item.EgressMode
	result["egress_label"] = item.EgressLabel
	result["has_proxy"] = item.HasProxy
	return result
}

func (s *Server) historicalMonitorRecords(limit int) ([]map[string]any, []map[string]any) {
	items := s.loadCallLogs()
	records := []map[string]any{}
	events := []map[string]any{}
	for index := len(items) - 1; index >= 0 && len(records) < limit; index-- {
		item := items[index]
		if !strings.EqualFold(stringValue(item["type"]), "call") {
			continue
		}
		detail := mapValue(item["detail"])
		callID := firstNonEmpty(stringValue(detail["call_id"]), stringValue(item["id"]))
		if callID == "" {
			continue
		}
		monitor := mapValue(detail["monitor"])
		// Legacy Go records only contained start/completed and no timings. They
		// cannot drive the server-compatible slow-request visualization, so keep
		// them in log management but exclude them from realtime history.
		if len(mapValue(monitor["metrics"])) == 0 && len(mapValue(monitor["perf"])) == 0 && len(anyList(monitor["events"])) <= 2 {
			continue
		}
		record := map[string]any{
			"call_id":       callID,
			"endpoint":      stringValue(detail["endpoint"]),
			"model":         stringValue(detail["model"]),
			"summary":       stringValue(item["summary"]),
			"status":        firstNonEmpty(stringValue(detail["status"]), "success"),
			"stage":         firstNonEmpty(stringValue(monitor["stage"]), "completed"),
			"started_at":    stringValue(detail["started_at"]),
			"ended_at":      stringValue(detail["ended_at"]),
			"duration_ms":   monitorNumber(detail["duration_ms"]),
			"account_email": stringValue(detail["account_email"]),
			"account_id":    firstNonEmpty(stringValue(detail["provider_account_id"]), stringValue(monitor["account_id"])),
			"key_name":      firstNonEmpty(stringValue(detail["key_name"]), stringValue(monitor["key_name"])),
			"key_id":        firstNonEmpty(stringValue(detail["key_id"]), stringValue(monitor["key_id"])),
			"metrics":       mapValue(monitor["metrics"]),
			"perf":          mapValue(monitor["perf"]),
		}
		for _, key := range []string{"proxy_source", "proxy_hash", "egress_key", "egress_label", "egress_mode", "has_proxy", "error", "raw_error", "upstream_error", "upstream_message"} {
			if value, ok := monitor[key]; ok {
				record[key] = value
			} else if value, ok := detail[key]; ok {
				record[key] = value
			}
		}
		records = append(records, record)
		for _, rawEvent := range anyList(monitor["events"]) {
			event := mapValue(rawEvent)
			if len(event) == 0 {
				continue
			}
			event["call_id"] = callID
			if stringValue(event["model"]) == "" {
				event["model"] = record["model"]
			}
			events = append(events, event)
		}
	}
	return records, events
}

func anyList(value any) []any {
	if result, ok := value.([]any); ok {
		return result
	}
	return []any{}
}

func monitorNumber(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	default:
		return 0
	}
}

func monitorPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func monitorMetricValues(items []map[string]any, key string) []int64 {
	values := make([]int64, 0, len(items))
	for _, item := range items {
		value := monitorMetricValue(item, key)
		if value > 0 {
			values = append(values, value)
		}
	}
	return values
}

func monitorMetricValuesMap(items []map[string]any, keys []string) map[string][]int64 {
	result := make(map[string][]int64, len(keys))
	for _, key := range keys {
		result[key] = monitorMetricValues(items, key)
	}
	return result
}

func monitorMetricValue(item map[string]any, key string) int64 {
	if item == nil {
		return 0
	}
	if value := monitorNumber(item[key]); value > 0 {
		return value
	}
	if metrics := mapValue(item["metrics"]); len(metrics) > 0 {
		if value := monitorNumber(metrics[key]); value > 0 {
			return value
		}
	}
	if perf := mapValue(item["perf"]); len(perf) > 0 {
		if value := monitorNumber(perf[key]); value > 0 {
			return value
		}
	}
	return 0
}

func monitorCountsByField(items []map[string]any, valueFn func(map[string]any) string) map[string]any {
	result := map[string]any{}
	for _, item := range items {
		key := strings.TrimSpace(valueFn(item))
		if key == "" {
			continue
		}
		result[key] = monitorNumber(result[key]) + 1
	}
	return result
}

func monitorEgressKey(item map[string]any) string {
	if item == nil {
		return ""
	}
	if key := strings.TrimSpace(stringValue(item["active_egress_key"])); key != "" {
		return key
	}
	source := strings.TrimSpace(stringValue(item["proxy_source"]))
	label := strings.TrimSpace(firstNonEmpty(
		stringValue(item["egress_label"]),
		stringValue(item["proxy_group_id"]),
		stringValue(item["proxy_node_name"]),
		stringValue(item["proxy_node_id"]),
		stringValue(item["proxy_hash"]),
		stringValue(item["egress_key"]),
	))
	if source == "" && label == "" {
		return ""
	}
	if source == "" {
		source = "direct"
	}
	if label == "" {
		return source
	}
	return source + ":" + label
}

func monitorMetricLabel(key string) string {
	if label, ok := monitorMetricLabelMap[key]; ok {
		return label
	}
	return key
}

func monitorLooksLocallyBusy(item map[string]any) bool {
	if item == nil {
		return false
	}
	text := strings.ToLower(strings.Join([]string{
		stringValue(item["status"]),
		stringValue(item["error"]),
		stringValue(item["raw_error"]),
		stringValue(item["upstream_error"]),
		stringValue(item["upstream_message"]),
		stringValue(item["local_reason"]),
	}, " "))
	return strings.Contains(text, "busy") || strings.Contains(text, "reject") || strings.Contains(text, "too many open connections")
}

func (s *Server) appendCallLog(record monitorRecord, statusCode int, requestShape any, responseBody []byte, errorText string) {
	if s == nil || strings.TrimSpace(record.CallID) == "" {
		return
	}
	if errText := strings.TrimSpace(errorText); errText != "" {
		errorText = errText
	}
	startedAt := time.UnixMilli(record.StartedAt).UTC()
	endedAt := time.UnixMilli(record.EndedAt).UTC()
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	detail := map[string]any{
		"call_id":       record.CallID,
		"endpoint":      record.Endpoint,
		"model":         record.Model,
		"status":        record.Status,
		"stage":         record.Stage,
		"duration_ms":   record.Duration,
		"status_code":   statusCode,
		"started_at":    startedAt.Format(time.RFC3339),
		"ended_at":      endedAt.Format(time.RFC3339),
		"progress":      record.Progress,
		"request_shape": requestShape,
		"monitor":       map[string]any{"stage": record.Stage, "metrics": record.Metrics, "perf": record.Perf, "events": record.Events, "proxy_source": record.ProxySource, "egress_mode": record.EgressMode, "egress_label": record.EgressLabel, "has_proxy": record.HasProxy, "account_email": record.AccountEmail, "account_id": record.AccountID, "key_name": record.KeyName, "key_id": record.KeyID},
	}
	if record.AccountEmail != "" {
		detail["account_email"] = record.AccountEmail
	}
	if record.AccountID != "" {
		detail["provider_account_id"] = record.AccountID
	}
	if record.KeyName != "" {
		detail["key_name"] = record.KeyName
	}
	if record.KeyID != "" {
		detail["key_id"] = record.KeyID
	}
	if shape, ok := requestShape.(map[string]any); ok {
		detail["request_meta"] = map[string]any{"size": shape["size"], "image_url_parts": shape["image_url_parts"], "data_url_images": shape["data_url_images"]}
	}
	if record.RequestMeta != nil {
		detail["request_meta"] = record.RequestMeta
	}
	if summary := strings.TrimSpace(record.Summary); summary != "" {
		detail["request_text"] = summary
		detail["request_text_full"] = summary
		if len(summary) > 180 {
			detail["request_text_truncated"] = true
		}
	}
	if errorText != "" {
		detail["error"] = errorText
		detail["raw_error"] = errorText
		detail["upstream_error"] = errorText
	}
	if outputs := responseImageOutputs(responseBody); len(outputs) > 0 {
		detail["output_images"] = outputs
		detail["image_urls"] = outputs
	}
	entry := map[string]any{
		"id":      record.CallID,
		"time":    endedAt.Format(time.RFC3339),
		"type":    "call",
		"summary": firstNonEmpty(strings.TrimSpace(record.Summary), record.Endpoint, record.Model, record.CallID),
		"detail":  detail,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return
	}
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	s.logMu.Lock()
	defer s.logMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

func responseImageOutputs(raw []byte) []map[string]string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	out := []map[string]string{}
	appendURL := func(value string) {
		u := strings.TrimSpace(value)
		if u == "" {
			return
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed == nil {
			return
		}
		name := filepath.Base(parsed.Path)
		if id := parsed.Query().Get("id"); id != "" {
			name = id
		}
		out = append(out, map[string]string{"url": u, "filename": name})
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if k == "url" {
					appendURL(stringValue(item))
				} else {
					walk(item)
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case string:
			for _, part := range strings.Fields(x) {
				if strings.Contains(part, "/v1/files/image?id=") || strings.Contains(part, "/images/") {
					u := strings.Trim(part, "\"")
					if marker := strings.Index(u, "]("); marker >= 0 {
						u = u[marker+2:]
						u = strings.TrimSuffix(u, ")")
					} else {
						u = strings.Trim(u, "![]()")
					}
					appendURL(u)
				}
			}
		}
	}
	walk(value)
	return out
}

func monitorStageLabel(stage string) string {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "handler_submitted":
		return "等待入口"
	case "handler_started":
		return "入口执行"
	case "stream_first_item":
		return "读取首包"
	case "image_getting_account", "image_account_lookup", "image_account_wait_slow":
		return "等待账号"
	case "image_egress_waiting":
		return "等待出口"
	case "image_egress_ready":
		return "出口就绪"
	case "image_uploading":
		return "上传图片"
	case "image_bootstrapping":
		return "初始化上游"
	case "image_getting_token":
		return "获取令牌"
	case "image_preparing_conversation":
		return "准备会话"
	case "image_starting_generation":
		return "等待上游首包"
	case "image_generating":
		return "上游生成中"
	case "image_stream_resolve_start":
		return "解析上游结果"
	case "image_resolving", "image_resolve_done":
		return "解析图片"
	case "image_download_done":
		return "下载图片"
	case "image_retry_wait":
		return "重试等待"
	case "image_single_done":
		return "单图完成"
	case "queued":
		return "排队"
	case "running":
		return "运行中"
	case "completed", "success":
		return "完成"
	case "failed", "error":
		return "失败"
	case "cancelled", "canceled":
		return "取消"
	default:
		return stage
	}
}

var monitorMetricLabelMap = map[string]string{
	"handler_queue_ms":        "等待入口",
	"stream_first_queue_ms":   "首包",
	"account_wait_ms":         "等待账号",
	"egress_wait_ms":          "等待出口",
	"egress_acquire_ms":       "出口租约",
	"upload_ms":               "上传",
	"bootstrap_ms":            "初始化",
	"requirements_ms":         "令牌",
	"prepare_conversation_ms": "准备",
	"generation_start_ms":     "启动",
	"http_dns_ms":             "HTTP DNS",
	"http_tcp_ms":             "HTTP TCP",
	"http_tls_ms":             "HTTP TLS",
	"http_wait_ms":            "HTTP 等待",
	"http_ttfb_ms":            "HTTP 首包",
	"sse_first_event_ms":      "SSE 首事件",
	"sse_max_gap_ms":          "SSE 最大空窗",
	"sse_last_gap_ms":         "SSE 收尾空窗",
	"conversation_stream_ms":  "上游生成",
	"stream_error_ms":         "上游断流",
	"resolve_ms":              "解析/轮询",
	"download_ms":             "下载",
	"retry_wait_ms":           "重试等待",
	"response_ms":             "响应整理",
	"stream_ms":               "单图内部",
	"total_ms":                "单图总耗时",
}

var monitorMonitorMetricKeys = []string{
	"handler_queue_ms",
	"stream_first_queue_ms",
	"account_wait_ms",
	"egress_wait_ms",
	"egress_acquire_ms",
	"upload_ms",
	"bootstrap_ms",
	"requirements_ms",
	"prepare_conversation_ms",
	"generation_start_ms",
	"http_dns_ms",
	"http_tcp_ms",
	"http_tls_ms",
	"http_wait_ms",
	"http_ttfb_ms",
	"sse_first_event_ms",
	"sse_max_gap_ms",
	"sse_last_gap_ms",
	"conversation_stream_ms",
	"stream_error_ms",
	"resolve_ms",
	"download_ms",
	"retry_wait_ms",
	"response_ms",
	"stream_ms",
	"total_ms",
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	accounts, _ := s.store.AccountList()
	active := 0
	for _, item := range accounts {
		if boolValue(item["enabled"], true) && !strings.EqualFold(stringValue(item["status"]), "disabled") {
			active++
		}
	}
	healthy := active > 0
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   map[bool]string{true: "ok", false: "degraded"}[healthy],
		"healthy":  healthy,
		"runtime":  "go",
		"version":  s.cfg.Version,
		"accounts": map[string]any{"total": len(accounts), "active": active, "healthy": healthy},
		"storage":  map[string]any{"backend": "json", "health": "ok", "images": mediaStats(s.cfg.ImageDataDir), "videos": mediaStats(s.cfg.VideoDataDir)},
		"logs":     map[string]any{"monitor": s.monitorSnapshotWithHistory()},
	})
}

func mediaStats(root string) map[string]any {
	var count int
	var size int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			count++
			size += info.Size()
		}
		return nil
	})
	return map[string]any{"count": count, "size_bytes": size}
}
