package httpapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) metaUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": s.cfg.Version, "runtime": "go", "update_available": false})
}

func (s *Server) importAccountsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccountImport(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		Tokens   []string          `json:"tokens"`
		Accounts []map[string]any  `json:"accounts"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.Accounts) > 0 || len(request.Tokens) > 0 {
		accounts := make([]map[string]any, 0, len(request.Accounts))
		for _, account := range request.Accounts {
			accounts = append(accounts, normalizeImportedAccount(account))
		}
		added, skipped, items, err := s.store.AddAccounts(request.Tokens, accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped, "items": accountsForAPI(items)})
		return
	}
	target, err := url.Parse(strings.TrimSpace(request.URL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "valid import URL is required", "invalid_request_error")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	resp, err := s.requestClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "account import endpoint returned "+resp.Status, "upstream_error")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, http.StatusBadGateway, "account import response is not JSON", "upstream_error")
		return
	}
	tokens, accounts := importedAccountValues(payload)
	for index := range accounts {
		accounts[index] = normalizeImportedAccount(accounts[index])
	}
	added, skipped, items, err := s.store.AddAccounts(tokens, accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped, "items": accountsForAPI(items)})
}

func (s *Server) requireAccountImport(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAdminRequest(r) {
		return true
	}
	config, err := s.store.Config()
	if err == nil {
		settings := mapValue(config["account_import_api"])
		expected := strings.TrimSpace(stringValue(settings["key"]))
		provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if boolValue(settings["enabled"], false) && expected != "" && len(expected) == len(provided) && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1 {
			return true
		}
	}
	writeError(w, http.StatusUnauthorized, "账号导入 API 未启用或密钥无效", "authentication_error")
	return false
}

func normalizeImportedAccount(account map[string]any) map[string]any {
	result := cloneMap(account)
	if token := firstNonEmpty(stringValue(result["access_token"]), stringValue(result["accessToken"]), stringValue(result["token"])); token != "" {
		result["access_token"] = token
	}
	if password := firstNonEmpty(stringValue(result["login_password"]), stringValue(result["password"]), stringValue(result["account_password"])); password != "" {
		result["login_password"] = password
	}
	if secret := firstNonEmpty(stringValue(result["two_factor_secret"]), stringValue(result["totp_secret"]), stringValue(result["two_fa"]), stringValue(result["2fa"]), stringValue(result["2fa_secret"])); secret != "" {
		result["two_factor_secret"] = secret
	}
	delete(result, "accessToken")
	delete(result, "account_password")
	delete(result, "totp_secret")
	delete(result, "two_fa")
	delete(result, "2fa")
	delete(result, "2fa_secret")
	return result
}

func importedAccountValues(value any) ([]string, []map[string]any) {
	tokens := []string{}
	accounts := []map[string]any{}
	var walk func(any)
	walk = func(item any) {
		switch typed := item.(type) {
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case map[string]any:
			if token := firstNonEmpty(stringValue(typed["access_token"]), stringValue(typed["token"])); token != "" {
				accounts = append(accounts, typed)
				return
			}
			for _, key := range []string{"items", "accounts", "data", "results"} {
				if child, ok := typed[key]; ok {
					walk(child)
				}
			}
		case string:
			if clean := strings.TrimSpace(typed); clean != "" {
				tokens = append(tokens, clean)
			}
		}
	}
	walk(value)
	return tokens, accounts
}

func (s *Server) backupTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := os.MkdirAll(s.backupDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	file, err := os.CreateTemp(s.backupDir(), ".probe-*")
	if err == nil {
		name := file.Name()
		err = file.Close()
		_ = os.Remove(name)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backend": "local", "directory": s.backupDir()})
}

func (s *Server) imageStorageTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	for _, directory := range []string{s.cfg.ImageDataDir, s.cfg.VideoDataDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backend": "local"})
}

func (s *Server) imageStorageSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	items := append(listMediaItems(s.cfg.ImageDataDir, "image", ""), listMediaItems(s.cfg.VideoDataDir, "video", "video/")...)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "synced": len(items), "total_size": mediaItemsSize(items)})
}

func (s *Server) proxyProfileByID(w http.ResponseWriter, r *http.Request) {
	s.proxyResourceByID(w, r, "proxy_profiles", "/api/proxy/profiles/", "profiles")
}

func (s *Server) proxyGroupByID(w http.ResponseWriter, r *http.Request) {
	s.proxyResourceByID(w, r, "proxy_groups", "/api/proxy/groups/", "groups")
}

func (s *Server) proxyResourceByID(w http.ResponseWriter, r *http.Request, configKey, prefix, responseKey string) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	refresh := strings.HasSuffix(id, "/subscription/refresh")
	if refresh {
		id = strings.TrimSuffix(id, "/subscription/refresh")
	}
	if refresh && r.Method == http.MethodPost {
		result, err := s.refreshProxyGroupSubscription(id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "proxy subscription refresh failed", "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	values := mapList(cfg[configKey])
	next := make([]map[string]any, 0, len(values))
	found := false
	for _, item := range values {
		if stringValue(item["id"]) == id {
			found = true
			continue
		}
		next = append(next, item)
	}
	if !found {
		writeError(w, http.StatusNotFound, "proxy resource not found", "not_found")
		return
	}
	updated, err := s.store.UpdateConfig(configKey, next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, responseKey: mapList(updated[configKey])})
}

func (s *Server) iCloudClaimStatusSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ICLOUD_PRIVACY_MAIL_BASE_URL")), "/")
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, "iCloud privacy mail service is not configured", "not_configured")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/api/v1/mailboxes/claim-status", bytes.NewReader(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("ICLOUD_PRIVACY_MAIL_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-API-Key", key)
	}
	resp, err := s.requestClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
}

func (s *Server) internalImageMonitor(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal/image-monitor/")
	callID := stringValue(body["call_id"])
	switch path {
	case "start":
		s.monitor.start(callID, stringValue(body["endpoint"]), stringValue(body["model"]), stringValue(body["summary"]))
	case "stage":
		s.monitor.update(callID, stringValue(body["event"]), intValue(body["progress"]), stringValue(body["error"]))
		s.monitor.enrich(callID, body)
	case "finish":
		s.monitor.finish(callID, firstNonEmpty(stringValue(body["status"]), "success"), stringValue(body["model"]), stringValue(body["summary"]), stringValue(body["error"]))
	default:
		writeError(w, http.StatusNotFound, "monitor endpoint not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) internalImageScheduler(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal/image-scheduler/")
	if path == "status" && r.Method == http.MethodGet {
		s.schedulerMu.Lock()
		items := make([]map[string]any, 0, len(s.schedulerLeases))
		for _, item := range s.schedulerLeases {
			items = append(items, item)
		}
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"runtime": "go", "active": len(items), "reservations": items})
		return
	}
	if path == "reserve" && r.Method == http.MethodPost {
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		id := "reservation_" + randomID()
		item := map[string]any{"id": id, "reservation_id": id, "model": firstNonEmpty(stringValue(body["model"]), "gpt-image-2"), "status": "reserved", "created_at": time.Now().UTC()}
		s.schedulerMu.Lock()
		s.schedulerLeases[id] = item
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, item)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "scheduler endpoint not found", "not_found")
		return
	}
	id, action := parts[0], parts[1]
	s.schedulerMu.Lock()
	item := s.schedulerLeases[id]
	if item == nil {
		s.schedulerMu.Unlock()
		writeError(w, http.StatusNotFound, "reservation not found", "not_found")
		return
	}
	if action == "release" {
		delete(s.schedulerLeases, id)
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reservation_id": id, "status": "released"})
		return
	}
	if action == "execute" || action == "execute-edit" {
		item["status"] = "claimed"
		item["claimed_at"] = time.Now().UTC()
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lease": item, "execution": "handled directly by Go public image endpoint"})
		return
	}
	s.schedulerMu.Unlock()
	writeError(w, http.StatusNotFound, "scheduler endpoint not found", "not_found")
}

func (s *Server) internalCallLog(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	path := filepath.Join(s.cfg.RootDir, "logs", "calls.log")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	defer file.Close()
	line, _ := json.Marshal(map[string]any{"created_at": time.Now().UTC(), "payload": body})
	_, err = file.Write(append(line, '\n'))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) requireInternal(w http.ResponseWriter, r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("GO_IMAGE_SCHEDULER_KEY"))
	provided := strings.TrimSpace(r.Header.Get("X-Image-Scheduler-Key"))
	if expected != "" && provided != expected {
		writeError(w, http.StatusUnauthorized, "invalid internal scheduler key", "authentication_error")
		return false
	}
	return true
}
