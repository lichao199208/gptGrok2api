package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) registerAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/register")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		s.registerConfig(w)
	case (path == "" || path == "/") && r.Method == http.MethodPost:
		s.updateRegisterConfig(w, r)
	case path == "/start" && r.Method == http.MethodPost:
		s.setRegisterEnabled(w, true)
	case path == "/stop" && r.Method == http.MethodPost:
		s.setRegisterEnabled(w, false)
	case path == "/reset" && r.Method == http.MethodPost:
		s.resetRegister(w)
	case path == "/runtime" && r.Method == http.MethodGet:
		s.registerRuntimeStatus(w)
	case path == "/checkout-retries/stop" && r.Method == http.MethodPost:
		s.stopCheckoutRetries(w)
	case path == "/checkout-history/clear" && r.Method == http.MethodPost:
		s.clearCheckoutHistory(w)
	case path == "/outlook-pool/reset" && r.Method == http.MethodPost:
		s.resetOutlookPool(w, r)
	case path == "/outlook-pool/retry-selected" && r.Method == http.MethodPost:
		s.retryOutlookPool(w, r)
	case path == "/gptmail/status" && r.Method == http.MethodPost:
		s.gptMailStatus(w, r, false)
	case path == "/gptmail/refresh-key" && r.Method == http.MethodPost:
		s.gptMailStatus(w, r, true)
	case path == "/openai/survival" && r.Method == http.MethodGet:
		s.openAISurvival(w)
	case path == "/openai/survival" && r.Method == http.MethodPost:
		s.updateOpenAISurvival(w, r)
	case path == "/openai/survival/run" && r.Method == http.MethodPost:
		s.runOpenAISurvival(w)
	case path == "/grok/accounts" && r.Method == http.MethodGet:
		s.listRegisteredGrokAccounts(w, r)
	case strings.HasPrefix(path, "/grok/accounts/") && strings.HasSuffix(path, "/credentials") && r.Method == http.MethodGet:
		s.registeredGrokCredentials(w, strings.TrimSuffix(strings.TrimPrefix(path, "/grok/accounts/"), "/credentials"))
	case path == "/grok/probe-polling" && r.Method == http.MethodPost:
		s.setGrokProbePolling(w, r)
	case path == "/grok/accounts/runtime/snapshot" && r.Method == http.MethodPost:
		s.refreshGrokRuntimeSnapshot(w)
	case path == "/grok/accounts/sync" && r.Method == http.MethodPost:
		s.syncRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/oauth/authorize" && r.Method == http.MethodPost:
		s.authorizeRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/runtime/refresh" && r.Method == http.MethodPost:
		s.refreshRegisteredGrokRuntime(w, r)
	case path == "/grok/accounts/runtime/verify" && r.Method == http.MethodPost:
		s.verifyRegisteredGrokRuntime(w, r)
	case path == "/grok/accounts/runtime/disabled" && r.Method == http.MethodPost:
		s.disableRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts" && r.Method == http.MethodDelete:
		s.deleteRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/export" && r.Method == http.MethodGet:
		s.exportRegisteredGrokAccounts(w, r, nil)
	case path == "/grok/accounts/export" && r.Method == http.MethodPost:
		s.exportSelectedGrokAccounts(w, r)
	case path == "/grok/accounts/export-sso" && r.Method == http.MethodGet:
		s.exportGrokSSO(w, nil)
	case path == "/grok/accounts/export-sso" && r.Method == http.MethodPost:
		s.exportSelectedGrokSSO(w, r)
	case path == "/events" && r.Method == http.MethodGet:
		s.registerEvents(w, r)
	default:
		writeError(w, http.StatusNotFound, "register endpoint not found", "not_found")
	}
}

func (s *Server) registerConfig(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"register": s.registerStore.Get()})
}

func (s *Server) updateRegisterConfig(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if !decodeJSON(w, r, &updates) {
		return
	}
	value, err := s.registerStore.Update(updates)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) setRegisterEnabled(w http.ResponseWriter, enabled bool) {
	if enabled {
		config := s.registerStore.Get()
		target := stringValue(config["target"])
		if target == "" {
			target = "grok"
		}
		if err := s.registerRuntime.Start(target); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":       false,
				"runtime":  s.registerRuntime.Status(target),
				"register": config,
			})
			return
		}
	} else {
		s.registerRuntime.Stop()
	}
	value, err := s.registerStore.SetEnabled(enabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) resetRegister(w http.ResponseWriter) {
	value, err := s.registerStore.Reset()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) registerAction(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "runtime": "go", "error": message})
}

func (s *Server) resetOutlookPool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	scope := strings.ToLower(strings.TrimSpace(body.Scope))
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "unused" && scope != "failed" && scope != "retryable" && scope != "invalid" {
		writeError(w, 400, "invalid Outlook pool reset scope", "invalid_request_error")
		return
	}
	value, err := s.registerStore.ResetOutlookPoolState(scope)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"register": value, "runtime_available": false, "runtime_error": "Outlook token/Graph registration worker is not configured in Go runtime"})
}

func (s *Server) retryOutlookPool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string   `json:"provider_id"`
		MailboxIDs []string `json:"mailbox_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ProviderID) == "" || len(uniqueStrings(body.MailboxIDs)) == 0 {
		writeError(w, 400, "provider_id and mailbox_ids are required", "invalid_request_error")
		return
	}
	value, err := s.registerStore.QueueOutlookRetry(body.ProviderID, uniqueStrings(body.MailboxIDs))
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "register": value, "runtime_available": false, "runtime_error": "Outlook token/Graph registration worker is not configured in Go runtime"})
}

func (s *Server) gptMailStatus(w http.ResponseWriter, r *http.Request, refreshKey bool) {
	var body struct {
		Provider map[string]any `json:"provider"`
		Force    *bool          `json:"force"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	config := s.registerStore.Get()
	mail := mapValue(config["mail"])
	providerConfig := body.Provider
	if providerConfig == nil {
		providerConfig = firstGPTMailConfig(mail)
	}
	if providerConfig == nil {
		writeError(w, http.StatusBadRequest, "未找到 GPTMail 邮箱来源", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var result map[string]any
	var err error
	if refreshKey {
		force := body.Force == nil || *body.Force
		result, err = s.gptMail.RefreshPublicKey(ctx, providerConfig, force)
	} else {
		force := body.Force != nil && *body.Force
		result, err = s.gptMail.Status(ctx, providerConfig, force)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": result})
}

func firstGPTMailConfig(mail map[string]any) map[string]any {
	providers, _ := mail["providers"].([]any)
	for _, raw := range providers {
		item, ok := raw.(map[string]any)
		if ok && strings.EqualFold(stringValue(item["type"]), "gptmail") {
			return cloneMap(item)
		}
	}
	return nil
}

func (s *Server) stopCheckoutRetries(w http.ResponseWriter) {
	value, err := s.registerStore.StopCheckoutRetries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) clearCheckoutHistory(w http.ResponseWriter) {
	value, err := s.registerStore.ClearCheckoutHistory()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) setGrokProbePolling(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	status, err := s.registerStore.SetGrokProbeSchedulerEnabled(request.Enabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	select {
	case s.probeWake <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusOK, map[string]any{"probe_scheduler": status})
}

// refreshGrokRuntimeSnapshot imports the local registration archive and then
// probes each eligible SSO against the real Grok rate-limits endpoint.
func (s *Server) refreshGrokRuntimeSnapshot(w http.ResponseWriter) {
	items, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	result := map[string]any{
		"total":           len(items),
		"eligible":        0,
		"added":           0,
		"skipped":         0,
		"missing_sso":     0,
		"disabled":        0,
		"quota_refreshed": 0,
		"quota_failed":    0,
		"errors":          []any{},
	}
	type quotaJob struct {
		item  map[string]any
		token string
	}
	jobs := make([]quotaJob, 0, len(items))
	for _, item := range items {
		token := firstNonEmpty(stringValue(item["sso"]), stringValue(item["access_token"]), stringValue(item["token"]))
		status := strings.ToLower(strings.TrimSpace(stringValue(item["status"])))
		if token == "" {
			result["missing_sso"] = intValue(result["missing_sso"]) + 1
			continue
		}
		if status == "disabled" || !boolValue(item["enabled"], true) {
			result["disabled"] = intValue(result["disabled"]) + 1
			continue
		}
		if status != "" && status != "active" && status != "正常" {
			result["skipped"] = intValue(result["skipped"]) + 1
			continue
		}
		result["eligible"] = intValue(result["eligible"]) + 1
		added, skipped, _, addErr := s.store.AddAccounts(nil, []map[string]any{{"access_token": token, "source_type": "grok_sso", "type": "grok", "status": "正常", "enabled": true}})
		if addErr != nil {
			errors, _ := result["errors"].([]any)
			errors = append(errors, map[string]any{"id": stringValue(item["id"]), "error": addErr.Error()})
			result["errors"] = errors
			continue
		}
		result["added"] = intValue(result["added"]) + added
		result["skipped"] = intValue(result["skipped"]) + skipped
		_, _, _ = s.store.UpdateAccount(token, map[string]any{"source_type": "grok_sso", "type": "grok"})
		jobs = append(jobs, quotaJob{item: item, token: token})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	var resultMu sync.Mutex
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			quotas, quotaErr := s.grokQuota.RefreshToken(ctx, job.token, job.item)
			cancel()
			resultMu.Lock()
			defer resultMu.Unlock()
			if quotaErr != nil {
				result["quota_failed"] = intValue(result["quota_failed"]) + 1
				errorItems, _ := result["errors"].([]any)
				errorItems = append(errorItems, map[string]any{"id": stringValue(job.item["id"]), "stage": "quota", "error": safeRefreshError(quotaErr)})
				result["errors"] = errorItems
				status := "unknown"
				if errors.Is(quotaErr, provider.ErrGrokInvalidCredentials) {
					status = "invalid"
				}
				_, _, _ = s.store.UpdateAccount(job.token, map[string]any{"status": status, "last_refresh_error": safeRefreshError(quotaErr), "last_refresh_error_at": time.Now().UTC().Format(time.RFC3339)})
				return
			}
			result["quota_refreshed"] = intValue(result["quota_refreshed"]) + 1
			s.persistRuntimeToken(job.token, quotas, "active", "")
		}()
	}
	wg.Wait()
	errors, _ := result["errors"].([]any)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            len(errors) == 0,
		"refreshed":     true,
		"refreshing":    false,
		"error":         "",
		"summary":       result,
		"quota_refresh": map[string]any{"available": true, "refreshed": result["quota_refreshed"], "failed": result["quota_failed"]},
	})
}

func (s *Server) registerRuntimeStatus(w http.ResponseWriter) {
	config := s.registerStore.Get()
	target := stringValue(config["target"])
	if target == "" {
		target = "grok"
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": s.registerRuntime.Status(target)})
}

func (s *Server) openAISurvival(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"survival": s.survivalSnapshot()})
}

func (s *Server) updateOpenAISurvival(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if !decodeJSON(w, r, &updates) {
		return
	}
	value, err := s.registerStore.Update(map[string]any{"openai_survival": updates})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	select {
	case s.survivalWake <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusOK, map[string]any{"survival": value["openai_survival"]})
}

func (s *Server) listRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	items, allTotal, summary, err := s.registerStore.ListAccounts(r.URL.Query().Get("keyword"), r.URL.Query().Get("status"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	page := positiveInt(r.URL.Query().Get("page"), 1)
	pageSize := positiveInt(r.URL.Query().Get("page_size"), 100)
	if pageSize > 500 {
		pageSize = 500
	}
	start := (page - 1) * pageSize
	if start > len(items) {
		start = len(items)
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "total": len(items), "all_total": allTotal, "page": page, "page_size": pageSize, "items": items[start:end], "summary": summary, "runtime_available": false, "runtime_error": "Go registration runtime archive is local-only", "probe_scheduler": s.registerStore.GrokProbeSchedulerStatus()})
}

func (s *Server) registeredGrokCredentials(w http.ResponseWriter, id string) {
	item, found, err := s.registerStore.Credentials(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Grok 账号不存在或已删除", "not_found")
		return
	}
	if strings.TrimSpace(item["password"]) == "" {
		writeError(w, http.StatusConflict, "该 Grok 账号未保存登录密码", "conflict")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, item)
}

func decodeIDs(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var request struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &request) {
		return nil, false
	}
	set := map[string]bool{}
	ids := make([]string, 0, len(request.IDs))
	for _, id := range request.IDs {
		if id = strings.TrimSpace(id); id != "" && !set[id] {
			set[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required", "invalid_request_error")
		return nil, false
	}
	return ids, true
}

func (s *Server) syncRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	items, err := s.registerStore.GetAccounts(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	added, skipped := 0, 0
	for _, item := range items {
		token := strings.TrimSpace(stringValue(item["sso"]))
		if token == "" {
			continue
		}
		count, _, _, addErr := s.store.AddAccounts(nil, []map[string]any{{"access_token": token, "source_type": "grok_sso", "type": "grok", "status": "正常", "enabled": true}})
		if addErr != nil {
			continue
		}
		if count > 0 {
			added += count
		}
		_, _, _ = s.store.UpdateAccount(token, map[string]any{"source_type": "grok_sso", "type": "grok"})
		if count == 0 {
			skipped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync_state": "synced", "added": added, "skipped": skipped, "verification_pending": len(items) - added - skipped})
}

func (s *Server) authorizeRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		_, found, err := s.registerStore.Credentials(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		if !found {
			results = append(results, map[string]any{"id": id, "ok": false, "status": "failed", "error": "Grok 账号不存在"})
			continue
		}
		_, _ = s.registerStore.SetOAuthAuthorization(id, "queued", "")
		task := s.taskQueue.Submit("grok_oauth_authorize", map[string]any{"account_id": id})
		results = append(results, map[string]any{"id": id, "ok": true, "status": "queued", "job_id": task.ID})
	}
	queued, failed := 0, 0
	for _, result := range results {
		if result["ok"] == true {
			queued++
		} else {
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": map[string]int{"total": len(results), "queued": queued, "reused": 0, "skipped": 0, "failed": failed}, "results": results})
}

func (s *Server) disableRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs      []string `json:"ids"`
		Disabled bool     `json:"disabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	ids := uniqueIDs(request.IDs)
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required", "invalid_request_error")
		return
	}
	count, err := s.registerStore.SetDisabled(ids, request.Disabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": request.Disabled, "summary": map[string]int{"total": len(ids), "ok": count, "fail": len(ids) - count}})
}

func (s *Server) deleteRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	removed, err := s.registerStore.Delete(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "count": removed, "upstream_deleted": 0})
}

func (s *Server) exportRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request, ids []string) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "cpa" {
		s.exportCPA(w, ids)
		return
	}
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	payload := map[string]any{"exported_at": time.Now().UTC(), "proxies": []any{}, "accounts": items}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-accounts-sub2api.json"`)
	_, _ = w.Write(append(raw, '\n'))
}

func (s *Server) exportSelectedGrokAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs    []string `json:"ids"`
		Format string   `json:"format"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.EqualFold(request.Format, "cpa") {
		s.exportCPA(w, request.IDs)
		return
	}
	s.exportRegisteredGrokAccounts(w, r, request.IDs)
}

func (s *Server) exportCPA(w http.ResponseWriter, ids []string) {
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for index, item := range items {
		name := fmt.Sprintf("grok-account-%d.json", index+1)
		writer, createErr := archive.Create(name)
		if createErr != nil {
			writeError(w, http.StatusInternalServerError, createErr.Error(), "server_error")
			return
		}
		raw, _ := json.MarshalIndent(item, "", "  ")
		_, _ = writer.Write(append(raw, '\n'))
	}
	if err := archive.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-accounts-cpa.zip"`)
	_, _ = w.Write(buffer.Bytes())
}

func (s *Server) exportGrokSSO(w http.ResponseWriter, ids []string) {
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if token := strings.TrimSpace(stringValue(item["sso"])); token != "" {
			lines = append(lines, token)
		}
	}
	if len(lines) == 0 {
		writeError(w, http.StatusBadRequest, "暂无可导出的 Grok SSO 账号", "invalid_request_error")
		return
	}
	sort.Strings(lines)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-sso.txt"`)
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

func (s *Server) exportSelectedGrokSSO(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	s.exportGrokSSO(w, ids)
}

func (s *Server) registerEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := ""
	for {
		payload := map[string]any{"register": s.registerStore.Get()}
		raw, _ := json.Marshal(payload)
		if string(raw) != last {
			writeSSE(w, payload)
			last = string(raw)
			if flusher != nil {
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func uniqueIDs(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
