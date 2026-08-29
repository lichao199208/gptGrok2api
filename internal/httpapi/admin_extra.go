package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// The Python service stores these resources as small JSON documents. Keep the
// Go implementation deliberately file based so the endpoints remain useful
// in the same volume layout and do not need an additional database.

type imageTaskState struct {
	ID         string           `json:"id"`
	OwnerID    string           `json:"-"`
	Status     string           `json:"status"`
	Mode       string           `json:"mode"`
	Model      string           `json:"model"`
	N          int              `json:"n"`
	Size       string           `json:"size,omitempty"`
	Quality    string           `json:"quality,omitempty"`
	Prompt     string           `json:"-"`
	Images     [][]byte         `json:"-"`
	ImageNames []string         `json:"-"`
	Data       []map[string]any `json:"data,omitempty"`
	Error      string           `json:"error,omitempty"`
	CreatedAt  string           `json:"created_at"`
	UpdatedAt  string           `json:"updated_at"`
}

type editableFileTaskState struct {
	ID        string         `json:"id"`
	TaskID    string         `json:"taskId,omitempty"`
	Owner     string         `json:"owner_id"`
	Status    string         `json:"status"`
	Kind      string         `json:"kind"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Error     string         `json:"error,omitempty"`
	Result    map[string]any `json:"result,omitempty"`
	Prompt    string         `json:"-"`
	Images    []string       `json:"-"`
	StartedAt time.Time      `json:"-"`
}

func (s *Server) thirdPartyApps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, _ := s.store.Config()
	apps := map[string]any{}
	if configured, ok := cfg["third_party_apps"].(map[string]any); ok {
		apps = configured
	}
	writeJSON(w, http.StatusOK, map[string]any{"third_party_apps": apps})
}

func (s *Server) modelCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	chat, images, videos := []string{}, []string{}, []string{}
	for _, item := range s.catalog {
		if !item.Enabled {
			continue
		}
		switch {
		case item.Capability&model.Chat != 0:
			chat = append(chat, item.ID)
		case item.Capability&model.Image != 0 || item.Capability&model.ImageEdit != 0:
			images = append(images, item.ID)
		case item.Capability&model.Video != 0:
			videos = append(videos, item.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "model_catalog", "chat_models": chat, "image_models": images,
		"image_edit_models": images, "video_models": videos, "models": []any{},
		"all_models":             append(append(append([]string{}, chat...), images...), videos...),
		"source":                 map[string]string{"chat": "go", "image": "go", "grok": "unavailable"},
		"openai_models_endpoint": "/v1/models",
	})
}

func (s *Server) proxyRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		if _, err := s.store.UpdateConfig("proxy_runtime", updates); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
	}
	cfg, _ := s.store.Config()
	runtime := map[string]any{}
	if value, ok := cfg["proxy_runtime"].(map[string]any); ok {
		runtime = value
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": runtime, "status": s.proxyManager.Snapshot()})
}

func (s *Server) logsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 200, 1, 20000)
	offset := queryInt(r, "offset", 0, 0, 100000000)
	items := s.loadCallLogs()
	query := r.URL.Query()
	filtered := items[:0]
	for _, item := range items {
		if !logMatches(item, query) {
			continue
		}
		filtered = append(filtered, item)
	}
	// The Python endpoint returns newest entries first.
	sort.SliceStable(filtered, func(i, j int) bool { return fmt.Sprint(filtered[i]["time"]) > fmt.Sprint(filtered[j]["time"]) })
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": filtered[offset:end], "total": total, "limit": limit, "offset": offset})
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	targets := map[string]bool{}
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" {
			targets[id] = true
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"removed": 0})
		return
	}
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"removed": 0})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	kept := make([]string, 0)
	removed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) == nil && targets[stringValue(item["id"])] {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) loadCallLogs() []map[string]any {
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	file, err := os.Open(path)
	if err != nil {
		return []map[string]any{}
	}
	defer file.Close()
	items := []map[string]any{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item != nil {
			items = append(items, item)
		}
	}
	return items
}

func logMatches(item map[string]any, query url.Values) bool {
	text := func(key string) string { return strings.ToLower(strings.TrimSpace(fmt.Sprint(item[key]))) }
	for _, key := range []string{"type", "status", "endpoint", "model", "account", "conversation_id"} {
		want := strings.TrimSpace(query.Get(key))
		if want != "" && !strings.Contains(text(key), strings.ToLower(want)) {
			return false
		}
	}
	if search := strings.ToLower(strings.TrimSpace(query.Get("search"))); search != "" {
		raw, _ := json.Marshal(item)
		if !strings.Contains(strings.ToLower(string(raw)), search) {
			return false
		}
	}
	return true
}

func (s *Server) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 300, 1, 2000)
	paths := []string{filepath.Join(s.cfg.DataDir, "runtime.log"), filepath.Join(s.cfg.DataDir, "app.log"), filepath.Join(s.cfg.RootDir, "logs", "runtime.log"), filepath.Join(s.cfg.RootDir, "logs", "app.log")}
	items := []map[string]any{}
	for _, path := range paths {
		items = append(items, tailRuntimeLog(path, limit)...)
		if len(items) >= limit {
			break
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "limit": limit, "sources": map[string]any{"memory": false, "files": paths}})
}

func tailRuntimeLog(path string, limit int) []map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	result := []map[string]any{}
	for i := len(lines) - 1; i >= 0 && len(result) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		level := "info"
		upper := strings.ToUpper(line)
		for _, candidate := range []string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"} {
			if strings.HasPrefix(upper, "["+candidate+"]") {
				level = strings.ToLower(candidate)
				line = strings.TrimSpace(line[len(candidate)+2:])
				break
			}
		}
		result = append(result, map[string]any{"id": fmt.Sprintf("file-%d", i), "time": "", "level": level, "message": line, "source": "file", "path": path})
	}
	return result
}

func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	items := s.loadPromptItems()
	sources := s.loadPromptSources()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "synced": len(items) > 0, "cached_source_count": len(sources), "enabled_source_count": len(sources)})
}

func (s *Server) loadPromptItems() []map[string]any {
	path := filepath.Join(s.cfg.RootDir, "services", "default_prompt_library.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	var doc struct {
		Prompts []map[string]any `json:"prompts"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return []map[string]any{}
	}
	return doc.Prompts
}

func (s *Server) loadPromptSources() []map[string]any {
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) == nil {
			if list, ok := doc["sources"].([]any); ok {
				result := []map[string]any{}
				for _, value := range list {
					if item, ok := value.(map[string]any); ok {
						result = append(result, item)
					}
				}
				if len(result) > 0 {
					return result
				}
			}
		}
	}
	return []map[string]any{{"id": "banana-prompt-quicker", "name": "Banana Prompt Quicker", "url": "local://default_prompt_library.json", "adapter": "json", "enabled": true, "built_in": true, "prompt_count": len(s.loadPromptItems())}}
}

func (s *Server) promptSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	sources := s.loadPromptSources()
	writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources)})
}

func (s *Server) promptSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/prompt-sources/"), "/")
	if id == "refresh" && r.Method == http.MethodPost {
		items := s.loadPromptItems()
		sources := s.loadPromptSources()
		writeJSON(w, 200, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if strings.HasSuffix(id, "/refresh") {
		id = strings.TrimSuffix(id, "/refresh")
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed", "invalid_request_error")
			return
		}
		writeJSON(w, 200, map[string]any{"items": s.loadPromptItems(), "prompt_count": len(s.loadPromptItems()), "sources": s.loadPromptSources(), "source_count": len(s.loadPromptSources()), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sources := s.loadPromptSources()
	found := false
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			found = true
			if body.Enabled != nil {
				source["enabled"] = *body.Enabled
			}
			source["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if !found {
		writeError(w, 404, "prompt source not found", "not_found")
		return
	}
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	raw, _ := json.MarshalIndent(map[string]any{"sources": sources}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources), "source": source})
			return
		}
	}
}

func queryInt(r *http.Request, key string, fallback, min, max int) int {
	value := fallback
	if raw := r.URL.Query().Get(key); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
			value = fallback
		}
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func (s *Server) retentionCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		LogRetentionDays   int `json:"log_retention_days"`
		ImageRetentionDays int `json:"image_retention_days"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	logDays, imageDays := body.LogRetentionDays, body.ImageRetentionDays
	if logDays <= 0 {
		logDays = 30
	}
	if imageDays <= 0 {
		imageDays = 15
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	result := s.cleanupRetentionFiles(logDays, imageDays, dryRun)
	writeJSON(w, 200, result)
}

func (s *Server) cleanupRetentionFiles(logDays, imageDays int, dryRun bool) map[string]any {
	now := time.Now()
	logCount, imageCount := 0, 0
	var logBytes, imageBytes int64
	visit := func(root string, older time.Duration, remove bool) (int, int64) {
		count := 0
		var size int64
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || now.Sub(info.ModTime()) <= older {
				return nil
			}
			count++
			size += info.Size()
			if remove {
				_ = os.Remove(path)
			}
			return nil
		})
		return count, size
	}
	logCount, logBytes = visit(filepath.Join(s.cfg.DataDir, "logs.jsonl"), time.Duration(logDays)*24*time.Hour, !dryRun)
	imageCount, imageBytes = visit(s.cfg.ImageDataDir, time.Duration(imageDays)*24*time.Hour, !dryRun)
	return map[string]any{"dry_run": dryRun, "logs": map[string]any{"removed": logCount, "removed_size_bytes": logBytes, "retention_days": logDays}, "images": map[string]any{"removed": imageCount, "removed_size_bytes": imageBytes, "retention_days": imageDays}, "total_removed": logCount + imageCount, "total_size_bytes": logBytes + imageBytes}
}

func (s *Server) accountCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AutoRemoveInvalidAccounts     *bool `json:"auto_remove_invalid_accounts"`
		AutoRemoveRateLimitedAccounts *bool `json:"auto_remove_rate_limited_accounts"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	removeInvalid, removeLimited := true, false
	if body.AutoRemoveInvalidAccounts != nil {
		removeInvalid = *body.AutoRemoveInvalidAccounts
	}
	if body.AutoRemoveRateLimitedAccounts != nil {
		removeLimited = *body.AutoRemoveRateLimitedAccounts
	}
	accounts, _ := s.store.AccountList()
	candidates := []string{}
	invalidCandidates := 0
	limitedCandidates := 0
	for _, account := range accounts {
		category := accountStatusCategory(account)
		// Only remove an abnormal account when the upstream has explicitly
		// rejected its credential (or marked the access token expired) and no
		// refresh token is available. Transient upstream errors must not delete
		// an otherwise recoverable account.
		invalid := removeInvalid && accountAutoRemoveInvalid(account)
		limited := removeLimited && category == "limited"
		if invalid || limited {
			candidates = append(candidates, accountToken(account))
			if invalid {
				invalidCandidates++
			}
			if limited {
				limitedCandidates++
			}
		}
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	removed := 0
	if !dryRun {
		removed, _, _ = s.store.DeleteAccounts(candidates)
	}
	total := len(candidates)
	if !dryRun {
		total = removed
	}
	writeJSON(w, 200, map[string]any{
		"dry_run":             dryRun,
		"checked":             len(accounts),
		"candidates":          len(candidates),
		"removed":             removed,
		"total_removed":       total,
		"invalid":             invalidCandidates,
		"rate_limited":        limitedCandidates,
		"remove_invalid":      removeInvalid,
		"remove_rate_limited": removeLimited,
	})
}

func (s *Server) imageTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.URL.Path == "/api/image-tasks/generations" && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	switch r.Method {
	case http.MethodGet:
		owner := s.authIdentity(r)
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		items := []map[string]any{}
		missing := []string{}
		s.imageTaskMu.RLock()
		if strings.TrimSpace(r.URL.Query().Get("ids")) == "" {
			for _, task := range s.imageTasks {
				if task.OwnerID == owner {
					items = append(items, imageTaskPublic(task))
				}
			}
			sort.SliceStable(items, func(i, j int) bool { return fmt.Sprint(items[i]["updated_at"]) > fmt.Sprint(items[j]["updated_at"]) })
		} else {
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				task, ok := s.imageTasks[id]
				if !ok || task.OwnerID != owner {
					missing = append(missing, id)
				} else {
					items = append(items, imageTaskPublic(task))
				}
			}
		}
		s.imageTaskMu.RUnlock()
		writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing, "quota_summary": s.imageQuota()})
	case http.MethodPost:
		var body struct {
			ClientTaskID string `json:"client_task_id"`
			Prompt       string `json:"prompt"`
			Model        string `json:"model"`
			N            int    `json:"n"`
			Size         string `json:"size"`
			Quality      string `json:"quality"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
			writeError(w, 400, "client_task_id and prompt are required", "invalid_request_error")
			return
		}
		if body.Model == "" {
			body.Model = "gpt-image-2"
		}
		if body.N == 0 {
			body.N = 1
		}
		if body.Quality == "" {
			body.Quality = "auto"
		}
		owner := s.authIdentity(r)
		task := &imageTaskState{ID: body.ClientTaskID, OwnerID: owner, Status: "queued", Mode: "generate", Model: body.Model, N: body.N, Size: body.Size, Quality: body.Quality, Prompt: body.Prompt, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		s.imageTaskMu.Lock()
		if previous := s.imageTasks[task.ID]; previous != nil && previous.OwnerID == owner {
			task = previous
		} else {
			s.imageTasks[task.ID] = task
			go s.runImageTask(task, r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
		}
		s.imageTaskMu.Unlock()
		writeJSON(w, 202, imageTaskPublic(task))
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) imageTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/image-tasks/"), "/")
	if strings.HasSuffix(path, "/resume-poll") {
		path = strings.TrimSuffix(path, "/resume-poll")
	}
	s.imageTaskMu.RLock()
	task, ok := s.imageTasks[path]
	var copyTask *imageTaskState
	if ok {
		copyValue := *task
		copyTask = &copyValue
	}
	s.imageTaskMu.RUnlock()
	if !ok || copyTask.OwnerID != s.authIdentity(r) {
		writeError(w, 404, "image task not found", "not_found")
		return
	}
	writeJSON(w, 200, imageTaskPublic(copyTask))
}

func (s *Server) imageTaskQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, s.imageQuota())
}

func (s *Server) imageTaskEdits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(112 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error")
		return
	}
	clientID := strings.TrimSpace(r.FormValue("client_task_id"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if clientID == "" || prompt == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required", "invalid_request_error")
		return
	}
	modelName := strings.TrimSpace(r.FormValue("model"))
	if modelName == "" {
		modelName = "grok-imagine-image-edit"
	}
	files := r.MultipartForm.File["image[]"]
	if len(files) == 0 {
		files = r.MultipartForm.File["image"]
	}
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "at least one image is required", "invalid_request_error")
		return
	}
	if len(files) > 7 {
		files = files[len(files)-7:]
	}
	images := make([][]byte, 0, len(files))
	names := make([]string, 0, len(files))
	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid image upload", "invalid_request_error")
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16<<20))
		_ = file.Close()
		if readErr != nil || len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "invalid image upload", "invalid_request_error")
			return
		}
		images = append(images, raw)
		names = append(names, filepath.Base(header.Filename))
	}
	owner := s.authIdentity(r)
	now := time.Now().UTC().Format(time.RFC3339)
	task := &imageTaskState{ID: clientID, OwnerID: owner, Status: "queued", Mode: "edit", Model: modelName, N: minInt(positiveInt(r.FormValue("n"), 1), 2), Size: "1024x1024", Quality: firstNonEmpty(r.FormValue("quality"), "auto"), Prompt: prompt, Images: images, ImageNames: names, CreatedAt: now, UpdatedAt: now}
	s.imageTaskMu.Lock()
	if previous := s.imageTasks[clientID]; previous != nil && previous.OwnerID == owner {
		task = previous
	} else {
		s.imageTasks[clientID] = task
		go s.runImageTask(task, r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
	}
	s.imageTaskMu.Unlock()
	writeJSON(w, http.StatusAccepted, imageTaskPublic(task))
}

func (s *Server) runImageTask(task *imageTaskState, authHeader, apiKey string) {
	var req *http.Request
	var target string
	var body io.Reader
	contentType := "application/json"
	if task.Mode == "edit" {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		_ = writer.WriteField("model", task.Model)
		_ = writer.WriteField("prompt", task.Prompt)
		_ = writer.WriteField("n", fmt.Sprint(task.N))
		_ = writer.WriteField("size", task.Size)
		_ = writer.WriteField("response_format", "url")
		for index, raw := range task.Images {
			name := "image.png"
			if index < len(task.ImageNames) && task.ImageNames[index] != "" {
				name = task.ImageNames[index]
			}
			part, err := writer.CreateFormFile("image[]", name)
			if err != nil {
				s.finishImageTaskError(task, err.Error())
				return
			}
			_, _ = part.Write(raw)
		}
		_ = writer.Close()
		body = bytes.NewReader(buffer.Bytes())
		contentType = writer.FormDataContentType()
		target = "http://internal/v1/images/edits"
	} else {
		raw, _ := json.Marshal(map[string]any{"model": task.Model, "prompt": task.Prompt, "n": task.N, "size": task.Size, "response_format": "url"})
		body = bytes.NewReader(raw)
		target = "http://internal/v1/images/generations"
	}
	parsed, _ := url.Parse(target)
	req = &http.Request{Method: http.MethodPost, URL: parsed, Header: make(http.Header), Body: io.NopCloser(body), ContentLength: -1}
	req.Header.Set("Content-Type", contentType)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	recorder := &responseCapture{header: make(http.Header)}
	s.imageGenerations(recorder, req)
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if recorder.status >= 200 && recorder.status < 300 {
		var result map[string]any
		if json.Unmarshal(recorder.body.Bytes(), &result) == nil {
			if data, ok := result["data"].([]any); ok {
				for _, value := range data {
					if item, ok := value.(map[string]any); ok {
						task.Data = append(task.Data, item)
					}
				}
			}
			task.Status = "success"
		} else {
			task.Status = "error"
			task.Error = "invalid image response"
		}
	} else {
		task.Status = "error"
		var result map[string]any
		if json.Unmarshal(recorder.body.Bytes(), &result) == nil {
			if value, ok := result["error"].(map[string]any); ok {
				task.Error = stringValue(value["message"])
			}
		}
		if task.Error == "" {
			task.Error = strings.TrimSpace(recorder.body.String())
		}
		if task.Error == "" {
			task.Error = "image generation failed"
		}
	}
}

func (s *Server) finishImageTaskError(task *imageTaskState, message string) {
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	task.Status = "error"
	task.Error = message
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func imageTaskPublic(task *imageTaskState) map[string]any {
	value := map[string]any{"id": task.ID, "status": task.Status, "mode": task.Mode, "model": task.Model, "n": task.N, "size": task.Size, "quality": task.Quality, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt}
	if len(task.Data) > 0 {
		value["data"] = task.Data
	}
	if task.Error != "" {
		value["error"] = task.Error
	}
	return value
}

func (s *Server) imageQuota() map[string]any {
	accounts, _ := s.store.AccountList()
	active, abnormal, disabled := 0, 0, 0
	for _, account := range accounts {
		switch accountStatusCategory(account) {
		case "abnormal":
			abnormal++
		case "disabled":
			disabled++
		default:
			active++
		}
	}
	return map[string]any{"total_quota": 0, "unlimited_quota_count": 0, "unknown_quota_count": active, "active_accounts": active, "limited_accounts": 0, "abnormal_accounts": abnormal, "disabled_accounts": disabled, "providers": map[string]any{}, "available": active > 0}
}

func (s *Server) editableFileTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	owner := s.authIdentity(r)
	if r.Method == http.MethodGet {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		items := []map[string]any{}
		missing := []string{}
		s.fileTaskMu.RLock()
		if strings.TrimSpace(r.URL.Query().Get("ids")) == "" {
			for _, task := range s.fileTasks {
				if task.OwnerID() == owner {
					items = append(items, editableTaskPublic(task))
				}
			}
		} else {
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				task, ok := s.fileTasks[editableTaskKey(owner, id)]
				if !ok || task.OwnerID() != owner {
					missing = append(missing, id)
				} else {
					items = append(items, editableTaskPublic(task))
				}
			}
		}
		s.fileTaskMu.RUnlock()
		writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		ClientTaskID string   `json:"client_task_id"`
		Prompt       string   `json:"prompt"`
		Kind         string   `json:"kind"`
		Base64Images []string `json:"base64_images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Kind == "" {
		body.Kind = "ppt"
	}
	if body.Kind != "ppt" && body.Kind != "psd" {
		writeError(w, 400, "kind must be ppt or psd", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, 400, "prompt is required", "invalid_request_error")
		return
	}
	if body.ClientTaskID == "" {
		body.ClientTaskID = "file_" + randomID()
	}
	task := &editableFileTaskState{ID: body.ClientTaskID, TaskID: body.ClientTaskID, Owner: owner, Status: "queued", Kind: body.Kind, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if body.Kind == "psd" && len(body.Base64Images) == 0 {
		writeError(w, 400, "base64_images is empty", "invalid_request_error")
		return
	}
	task.Prompt = body.Prompt
	task.Images = body.Base64Images
	s.fileTaskMu.Lock()
	key := editableTaskKey(owner, task.ID)
	if old := s.fileTasks[key]; old != nil {
		task = old
	} else {
		s.fileTasks[key] = task
		s.saveEditableFileTasksLocked()
	}
	s.fileTaskMu.Unlock()
	go s.runEditableFileTask(task)
	writeJSON(w, 202, editableTaskPublic(task))
}

func (s *Server) editableFileTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/editable-file-tasks/"), "/")
	s.fileTaskMu.RLock()
	task, ok := s.fileTasks[editableTaskKey(s.authIdentity(r), id)]
	s.fileTaskMu.RUnlock()
	if !ok || task.OwnerID() != s.authIdentity(r) {
		writeError(w, 404, "editable file task not found", "not_found")
		return
	}
	writeJSON(w, 200, editableTaskPublic(task))
}

func (s *Server) pptGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "ppt")
}

func (s *Server) psdGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "psd")
}

func (s *Server) editableGeneration(w http.ResponseWriter, r *http.Request, kind string) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		ClientTaskID string   `json:"client_task_id"`
		Prompt       string   `json:"prompt"`
		Base64Images []string `json:"base64_images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request_error")
		return
	}
	if kind == "psd" && len(body.Base64Images) == 0 {
		writeError(w, http.StatusBadRequest, "base64_images is empty", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" {
		body.ClientTaskID = "file_" + randomID()
	}
	owner := s.authIdentity(r)
	task := &editableFileTaskState{ID: body.ClientTaskID, TaskID: body.ClientTaskID, Owner: owner, Status: "queued", Kind: kind, Prompt: body.Prompt, Images: body.Base64Images, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	s.fileTaskMu.Lock()
	key := editableTaskKey(owner, task.ID)
	if old := s.fileTasks[key]; old != nil {
		task = old
	} else {
		s.fileTasks[key] = task
		s.saveEditableFileTasksLocked()
	}
	s.fileTaskMu.Unlock()
	go s.runEditableFileTask(task)
	writeJSON(w, http.StatusAccepted, editableTaskPublic(task))
}

func (s *Server) runEditableFileTask(task *editableFileTaskState) {
	s.fileTaskMu.Lock()
	if task.Status != "queued" {
		s.fileTaskMu.Unlock()
		return
	}
	task.Status = "running"
	task.StartedAt = time.Now()
	task.UpdatedAt = task.StartedAt.UTC().Format(time.RFC3339)
	s.saveEditableFileTasksLocked()
	s.fileTaskMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	lease, err := s.accountPool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, nil, func(account accounts.Account) bool {
		if !isOpenAIAccount(account) {
			return false
		}
		plan := strings.ToLower(firstNonEmpty(stringValue(account.Fields["plan_type"]), stringValue(account.Fields["account_plan_type"]), stringValue(account.Fields["subscription_plan"])))
		return plan == "" || strings.Contains(plan, "plus") || strings.Contains(plan, "team") || strings.Contains(plan, "pro") || strings.Contains(plan, "enterprise")
	})
	if err == nil {
		defer s.accountPool.Release(lease)
	}
	inputs := []provider.OpenAIImageInput{}
	if err == nil {
		inputs, err = provider.DecodeEditableInputs(task.Images)
	}
	var exported provider.EditableExportResult
	if err == nil {
		exported, err = s.openAIImage.ExportEditable(ctx, lease.Account, task.Kind, task.Prompt, inputs)
	}
	if err == nil {
		result, saveErr := s.saveEditableExport(task, exported)
		if saveErr != nil {
			err = saveErr
		} else {
			s.fileTaskMu.Lock()
			task.Status = "success"
			task.Error = ""
			task.Result = result
			task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			s.saveEditableFileTasksLocked()
			s.fileTaskMu.Unlock()
			s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
			return
		}
	}
	if lease != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
	}
	s.fileTaskMu.Lock()
	task.Status = "error"
	task.Error = firstNonEmpty(errorString(err), "editable file task failed")
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.saveEditableFileTasksLocked()
	s.fileTaskMu.Unlock()
}

func (s *Server) saveEditableExport(task *editableFileTaskState, exported provider.EditableExportResult) (map[string]any, error) {
	ownerDigest := sha256.Sum256([]byte(task.Owner))
	taskDigest := sha256.Sum256([]byte(task.ID))
	relativeDir := filepath.ToSlash(filepath.Join(task.Kind, hex.EncodeToString(ownerDigest[:16]), hex.EncodeToString(taskDigest[:16])))
	root := filepath.Join(s.cfg.DataDir, "files")
	directory := filepath.Join(root, filepath.FromSlash(relativeDir))
	if !isWithin(root, directory) {
		return nil, fmt.Errorf("invalid editable file output path")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	primaryName := safeEditableOutputName(exported.Primary.Name, "."+task.Kind)
	if task.Kind == "ppt" {
		primaryName = safeEditableOutputName(exported.Primary.Name, ".pptx")
	}
	archiveName := safeEditableOutputName(exported.Archive.Name, ".zip")
	primaryPath := filepath.Join(directory, primaryName)
	archivePath := filepath.Join(directory, archiveName)
	if err := writeEditableOutput(primaryPath, exported.Primary.Data); err != nil {
		return nil, err
	}
	if err := writeEditableOutput(archivePath, exported.Archive.Data); err != nil {
		return nil, err
	}
	primaryRelative := filepath.ToSlash(filepath.Join(relativeDir, primaryName))
	archiveRelative := filepath.ToSlash(filepath.Join(relativeDir, archiveName))
	return map[string]any{
		"conversation_id": exported.ConversationID,
		"primary_url":     editableDownloadURL(primaryRelative, s.cfg.APIKey),
		"zip_url":         editableDownloadURL(archiveRelative, s.cfg.APIKey),
		"primary_name":    primaryName,
		"zip_name":        archiveName,
	}, nil
}

func safeEditableOutputName(name, fallbackExtension string) string {
	name = strings.TrimSpace(strings.ReplaceAll(filepath.Base(name), "\x00", ""))
	if name == "" {
		name = "artifact" + fallbackExtension
	}
	if filepath.Ext(name) == "" {
		name += fallbackExtension
	}
	return name
}

func writeEditableOutput(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("editable output is empty")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func editableDownloadURL(relative, secret string) string {
	return "/files/" + strings.ReplaceAll(url.PathEscape(relative), "%2F", "/") + "?signature=" + url.QueryEscape(editableFileSignature(secret, relative))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) downloadEditableFile(w http.ResponseWriter, r *http.Request) {
	relative := strings.TrimPrefix(r.URL.Path, "/files/")
	providedSignature := strings.TrimSpace(r.URL.Query().Get("signature"))
	expectedSignature := editableFileSignature(s.cfg.APIKey, relative)
	if providedSignature == "" || expectedSignature == "" || !hmac.Equal([]byte(providedSignature), []byte(expectedSignature)) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	root := filepath.Join(s.cfg.DataDir, "files")
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if !isWithin(root, path) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

func editableFileSignature(secret, relative string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(relative))
	return hex.EncodeToString(digest.Sum(nil))
}

func editableTaskPublic(task *editableFileTaskState) map[string]any {
	elapsed := 0
	if task.StartedAt.IsZero() {
		if created, err := time.Parse(time.RFC3339, task.CreatedAt); err == nil {
			elapsed = maxInt(0, int(time.Since(created).Seconds()))
		}
	} else {
		elapsed = maxInt(0, int(time.Since(task.StartedAt).Seconds()))
	}
	value := map[string]any{"id": task.ID, "taskId": task.TaskID, "status": task.Status, "kind": task.Kind, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "elapsed_seconds": elapsed}
	if task.Error != "" {
		value["error"] = task.Error
	}
	if task.Result != nil {
		value["result"] = task.Result
	}
	return value
}

func (t *editableFileTaskState) OwnerID() string { return t.Owner }

func editableTaskKey(owner, id string) string {
	return strings.TrimSpace(owner) + ":" + strings.TrimSpace(id)
}

func (s *Server) editableFileTasksPath() string {
	return filepath.Join(s.cfg.DataDir, "editable_file_tasks.json")
}

func (s *Server) loadEditableFileTasks() {
	raw, err := os.ReadFile(s.editableFileTasksPath())
	if err != nil {
		return
	}
	var envelope struct {
		Tasks []*editableFileTaskState `json:"tasks"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	changed := false
	s.fileTaskMu.Lock()
	for _, task := range envelope.Tasks {
		if task == nil || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Owner) == "" {
			continue
		}
		if task.TaskID == "" {
			task.TaskID = task.ID
		}
		if task.Status == "queued" || task.Status == "running" {
			task.Status = "error"
			task.Error = "服务已重启，未完成的任务已中断"
			task.UpdatedAt = now
			changed = true
		}
		s.fileTasks[editableTaskKey(task.Owner, task.ID)] = task
	}
	if changed {
		s.saveEditableFileTasksLocked()
	}
	s.fileTaskMu.Unlock()
}

func (s *Server) saveEditableFileTasksLocked() {
	items := make([]*editableFileTaskState, 0, len(s.fileTasks))
	for _, task := range s.fileTasks {
		items = append(items, task)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	raw, err := json.MarshalIndent(map[string]any{"tasks": items}, "", "  ")
	if err != nil {
		return
	}
	path := s.editableFileTasksPath()
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	temporary := path + ".tmp"
	if os.WriteFile(temporary, raw, 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, 400, "prompt is required", "invalid_request_error")
		return
	}
	if body.Model == "" {
		body.Model = "gpt-4o-search-preview"
	}
	request := protocol.ChatRequest{Model: body.Model, Messages: []protocol.Message{{Role: "user", Content: body.Prompt}}}
	route, ok := model.ResolveChat(body.Model)
	if !ok {
		route = model.ChatRoute{Mode: "normal", PoolCandidates: []string{"basic", "super", "heavy"}}
	}
	recorder := &responseCapture{header: make(http.Header)}
	if route.Console {
		s.completeConsoleChat(recorder, r, request)
	} else {
		s.completeChat(recorder, r, request, route)
	}
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if json.Unmarshal(recorder.body.Bytes(), &chat) != nil {
		writeError(w, 502, "invalid search response", "upstream_error")
		return
	}
	answer := ""
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				answer = stringValue(message["content"])
			}
		}
	}
	writeJSON(w, 200, map[string]any{"answer": answer, "sources": []any{}, "status": "completed", "model": body.Model})
}

func (s *Server) authIdentity(r *http.Request) string {
	token := s.auth.APIKey(r)
	if identity, ok := s.auth.Identity(token); ok && identity.ID != "" {
		return identity.ID
	}
	return "anonymous"
}
