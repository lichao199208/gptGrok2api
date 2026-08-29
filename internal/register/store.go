package register

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store contains the registration control plane and the Grok registration
// archive. The archive is deliberately separate from the runtime account pool.
type Store struct {
	configPath   string
	accountsPath string
	mu           sync.Mutex
}

func New(configPath, accountsPath string) *Store {
	return &Store{configPath: configPath, accountsPath: accountsPath}
}

func (s *Store) Get() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked()
}

func (s *Store) Update(updates map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.getLocked()
	for key, value := range updates {
		if key == "logs" || key == "grok_oauth_logs" {
			continue
		}
		current[key] = value
	}
	if err := writeJSON0600(s.configPath, current); err != nil {
		return nil, err
	}
	return cloneMap(current), nil
}

func (s *Store) Start() (map[string]any, error) {
	return s.setEnabled(true)
}

func (s *Store) Stop() (map[string]any, error) {
	return s.setEnabled(false)
}

func (s *Store) SetEnabled(enabled bool) (map[string]any, error) {
	return s.setEnabled(enabled)
}

func (s *Store) Reset() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := defaultConfig()
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

// StopCheckoutRetries cancels queued checkout work recorded by the Go
// control-plane. It does not modify account credentials or completed history.
func (s *Store) StopCheckoutRetries() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	items := checkoutTasks(value)
	for _, item := range items {
		status := strings.ToLower(stringValue(item["status"]))
		switch status {
		case "queued", "running", "retrying", "pending":
			item["status"] = "cancelled"
			item["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	value["checkout_tasks"] = items
	value["checkout_retries_active"] = false
	value["checkout_retry_job_count"] = 0
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

// ClearCheckoutHistory removes terminal checkout rows while preserving active
// queue state. This mirrors the safe part of the legacy control-plane action.
func (s *Store) ClearCheckoutHistory() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	items := checkoutTasks(value)
	active := map[string]bool{"queued": true, "running": true, "retrying": true, "pending": true}
	kept := make([]map[string]any, 0, len(items))
	removed := 0
	for _, item := range items {
		if active[strings.ToLower(stringValue(item["status"]))] {
			kept = append(kept, item)
		} else {
			removed++
		}
	}
	value["checkout_tasks"] = kept
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return map[string]any{"removed": removed, "register": cloneMap(value)}, nil
}

// GrokProbeSchedulerStatus exposes the persisted scheduler switch and runtime
// observations. Runtime fields are persisted so the admin UI survives restart.
func (s *Store) GrokProbeSchedulerStatus() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	probe := grokProbeConfig(s.getLocked())
	return map[string]any{
		"enabled":          boolValue(probe["enabled"], false),
		"running":          boolValue(probe["running"], false),
		"interval_minutes": positiveValue(probe["interval_minutes"], 60),
		"last_finished_at": stringValue(probe["last_finished_at"]),
		"next_run_at":      stringValue(probe["next_run_at"]),
		"available":        true,
		"error":            stringValue(probe["error"]),
	}
}

func (s *Store) SetGrokProbeSchedulerEnabled(enabled bool) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	probe := grokProbeConfig(value)
	probe["enabled"] = enabled
	value["grok_probe_scheduler"] = probe
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled":          enabled,
		"running":          boolValue(probe["running"], false),
		"interval_minutes": positiveValue(probe["interval_minutes"], 60),
		"last_finished_at": stringValue(probe["last_finished_at"]),
		"next_run_at":      stringValue(probe["next_run_at"]),
		"available":        true,
		"error":            stringValue(probe["error"]),
	}, nil
}

func (s *Store) UpdateGrokProbeSchedulerRuntime(running bool, nextRun, lastFinished, errorText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	probe := grokProbeConfig(value)
	probe["running"] = running
	probe["next_run_at"] = strings.TrimSpace(nextRun)
	probe["last_finished_at"] = strings.TrimSpace(lastFinished)
	probe["error"] = strings.TrimSpace(errorText)
	value["grok_probe_scheduler"] = probe
	return writeJSON0600(s.configPath, value)
}

// ResetOutlookPoolState persists the local mailbox-pool control state. The
// actual Outlook token/Graph worker is intentionally separate from this
// control plane, so reset remains useful even when no registration worker is
// running.
func (s *Store) ResetOutlookPoolState(scope string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	state := map[string]any{}
	if existing, ok := value["outlook_pool_state"].(map[string]any); ok {
		state = cloneMap(existing)
	}
	state["last_reset_scope"] = strings.TrimSpace(scope)
	state["last_reset_at"] = time.Now().UTC().Format(time.RFC3339)
	state["reset_count"] = intValue(state["reset_count"]) + 1
	state["retryable"] = []any{}
	state["invalid"] = []any{}
	if strings.EqualFold(strings.TrimSpace(scope), "unused") {
		state["unused_pruned_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	value["outlook_pool_state"] = state
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

func (s *Store) QueueOutlookRetry(providerID string, mailboxIDs []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	state := map[string]any{}
	if existing, ok := value["outlook_pool_state"].(map[string]any); ok {
		state = cloneMap(existing)
	}
	state["last_retry_provider_id"] = strings.TrimSpace(providerID)
	state["last_retry_at"] = time.Now().UTC().Format(time.RFC3339)
	state["retryable"] = stringAnyList(mailboxIDs)
	state["retry_executor_available"] = false
	state["retry_executor_error"] = "Outlook token/Graph registration worker is not configured in Go runtime"
	value["outlook_pool_state"] = state
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

func (s *Store) setEnabled(enabled bool) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	value["enabled"] = enabled
	stats, _ := value["stats"].(map[string]any)
	if stats == nil {
		stats = map[string]any{}
	}
	stats["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	value["stats"] = stats
	if err := writeJSON0600(s.configPath, value); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

func (s *Store) ListAccounts(keyword, status string) ([]map[string]any, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return nil, 0, nil, err
	}
	allTotal := len(items)
	filtered := make([]map[string]any, 0, len(items))
	needle := strings.ToLower(strings.TrimSpace(keyword))
	status = strings.ToLower(strings.TrimSpace(status))
	for _, item := range items {
		itemStatus := strings.ToLower(stringValue(item["status"]))
		if itemStatus == "" {
			itemStatus = "active"
		}
		if status != "" && status != "all" && itemStatus != status {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(fmt.Sprint(item["id"], " ", item["email"], " ", item["source_type"], " ", item["status"])), needle) {
			continue
		}
		filtered = append(filtered, publicAccount(item))
	}
	sort.Slice(filtered, func(i, j int) bool {
		return stringValue(filtered[i]["updated_at"]) > stringValue(filtered[j]["updated_at"])
	})
	summary := map[string]int{"total": allTotal, "active": 0, "pending": 0, "failed": 0, "disabled": 0}
	for _, item := range items {
		switch strings.ToLower(stringValue(item["status"])) {
		case "active":
			summary["active"]++
		case "disabled":
			summary["disabled"]++
		case "submission_failed", "failed":
			summary["failed"]++
		default:
			summary["pending"]++
		}
	}
	return filtered, allTotal, summary, nil
}

func (s *Store) GetAccounts(ids []string) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if set[stringValue(item["id"])] {
			result = append(result, cloneMap(item))
		}
	}
	return result, nil
}

func (s *Store) Credentials(id string) (map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return nil, false, err
	}
	for _, item := range items {
		if stringValue(item["id"]) != strings.TrimSpace(id) {
			continue
		}
		return map[string]string{
			"id":       stringValue(item["id"]),
			"email":    stringValue(item["email"]),
			"password": stringValue(item["password"]),
		}, true, nil
	}
	return nil, false, nil
}

func (s *Store) SetDisabled(ids []string, disabled bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return 0, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = true
	}
	count := 0
	for index, item := range items {
		if !set[stringValue(item["id"])] {
			continue
		}
		if disabled {
			item["status"] = "disabled"
		} else {
			item["status"] = "active"
		}
		item["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		items[index] = item
		count++
	}
	if count > 0 {
		err = writeJSON0600(s.accountsPath, items)
	}
	return count, err
}

// SetOAuthAuthorization records the state of an OAuth hand-off without
// storing device secrets in the public account view.
func (s *Store) SetOAuthAuthorization(id, status, errorText string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return false, err
	}
	for index, item := range items {
		if stringValue(item["id"]) != strings.TrimSpace(id) {
			continue
		}
		state, _ := item["oauth_authorization"].(map[string]any)
		if state == nil {
			state = map[string]any{}
		}
		state["status"] = strings.TrimSpace(status)
		state["attempted_at"] = time.Now().UTC().Format(time.RFC3339)
		state["error"] = strings.TrimSpace(errorText)
		attempts := 0
		if value, ok := state["attempts"].(float64); ok {
			attempts = int(value)
		}
		state["attempts"] = attempts + 1
		item["oauth_authorization"] = state
		item["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		items[index] = item
		return true, writeJSON0600(s.accountsPath, items)
	}
	return false, nil
}

// UpdateRuntime stores non-secret quota/probe observations next to a
// registration account. Credentials remain excluded from every public view.
func (s *Store) UpdateRuntime(id string, runtime, probe map[string]any, status, errorText string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return false, err
	}
	for index, item := range items {
		if stringValue(item["id"]) != strings.TrimSpace(id) {
			continue
		}
		if runtime != nil {
			item["runtime"] = cloneMap(runtime)
		}
		if probe != nil {
			item["probe"] = cloneMap(probe)
		}
		if strings.TrimSpace(status) != "" {
			item["status"] = strings.TrimSpace(status)
		}
		if strings.TrimSpace(errorText) != "" {
			errorText = strings.TrimSpace(errorText)
			if len(errorText) > 300 {
				errorText = errorText[:300]
			}
			item["last_runtime_error"] = errorText
		} else {
			delete(item, "last_runtime_error")
		}
		item["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		items[index] = item
		return true, writeJSON0600(s.accountsPath, items)
	}
	return false, nil
}

func (s *Store) Delete(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return 0, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = true
	}
	next := make([]map[string]any, 0, len(items))
	removed := 0
	for _, item := range items {
		if set[stringValue(item["id"])] {
			removed++
			continue
		}
		next = append(next, item)
	}
	if removed > 0 {
		err = writeJSON0600(s.accountsPath, next)
	}
	return removed, err
}

func (s *Store) ExportAccounts(ids []string) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadAccountsLocked()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return items, nil
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = true
	}
	result := make([]map[string]any, 0, len(ids))
	for _, item := range items {
		if set[stringValue(item["id"])] {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *Store) loadAccountsLocked() ([]map[string]any, error) {
	raw, err := os.ReadFile(s.accountsPath)
	if os.IsNotExist(err) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var value any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
	}
	if object, ok := value.(map[string]any); ok {
		value = object["items"]
	}
	list, ok := value.([]any)
	if !ok {
		return []map[string]any{}, nil
	}
	result := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result, nil
}

func (s *Store) getLocked() map[string]any {
	value := defaultConfig()
	raw, err := os.ReadFile(s.configPath)
	if err == nil {
		var loaded map[string]any
		if json.Unmarshal(raw, &loaded) == nil {
			for key, item := range loaded {
				value[key] = item
			}
		}
	}
	return value
}

func defaultConfig() map[string]any {
	return map[string]any{
		"enabled":                  false,
		"target":                   "grok",
		"total":                    0,
		"threads":                  2,
		"mode":                     "register",
		"target_quota":             0,
		"target_available":         0,
		"check_interval":           60,
		"stats":                    map[string]any{"done": 0, "success": 0, "fail": 0, "running": 0},
		"logs":                     []any{},
		"grok_oauth_logs":          []any{},
		"checkout_tasks":           []any{},
		"checkout_retries_active":  false,
		"checkout_retry_job_count": 0,
		"grok_probe_scheduler":     map[string]any{"enabled": false, "interval_minutes": 60, "last_finished_at": ""},
		"openai_survival":          map[string]any{"enabled": true, "interval_minutes": 60, "concurrency": 4, "refresh_codex_rt": true},
	}
}

func checkoutTasks(value map[string]any) []map[string]any {
	raw, _ := value["checkout_tasks"].([]any)
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			result = append(result, cloneMap(object))
		}
	}
	return result
}

func grokProbeConfig(value map[string]any) map[string]any {
	probe, _ := value["grok_probe_scheduler"].(map[string]any)
	if probe == nil {
		probe = map[string]any{}
	}
	return cloneMap(probe)
}

func boolValue(value any, fallback bool) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return fallback
}

func positiveValue(value any, fallback int) int {
	var result int
	switch typed := value.(type) {
	case float64:
		result = int(typed)
	case int:
		result = typed
	case string:
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &result)
	}
	if result < 1 {
		return fallback
	}
	return result
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		var result int
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &result)
		return result
	default:
		return 0
	}
}

func stringAnyList(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		if clean := strings.TrimSpace(value); clean != "" {
			result = append(result, clean)
		}
	}
	return result
}

func publicAccount(item map[string]any) map[string]any {
	result := cloneMap(item)
	delete(result, "password")
	delete(result, "sso")
	delete(result, "access_token")
	delete(result, "refresh_token")
	result["has_password"] = stringValue(item["password"]) != ""
	result["has_sso"] = stringValue(item["sso"]) != ""
	if email := stringValue(item["email"]); email != "" {
		result["email"] = maskEmail(email)
	}
	return result
}

func maskEmail(value string) string {
	parts := strings.SplitN(value, "@", 2)
	if len(parts) != 2 {
		return "***"
	}
	local := parts[0]
	if len(local) <= 2 {
		return local[:1] + "***@" + parts[1]
	}
	return local[:2] + "***" + local[len(local)-1:] + "@" + parts[1]
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func writeJSON0600(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty storage path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".register-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func NewID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("register-%d", time.Now().UnixNano())
	}
	return "register-" + hex.EncodeToString(raw)
}
