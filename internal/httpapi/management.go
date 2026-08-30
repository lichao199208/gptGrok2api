package httpapi

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error")
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "file is required", "invalid_request_error")
			return
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 16<<20))
		if err != nil || len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "file is empty or unreadable", "invalid_request_error")
			return
		}
		lease, err := s.accountPool.Reserve(r.Context(), []string{"basic", "super", "heavy"}, nil)
		if err != nil {
			writeError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error")
			return
		}
		defer s.accountPool.Release(lease)
		contentType := header.Header.Get("Content-Type")
		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(header.Filename))
			if contentType == "" {
				contentType = "application/octet-stream"
			}
		}
		id, uri, err := s.mediaProvider.Upload(r.Context(), lease.Account, header.Filename, contentType, base64.StdEncoding.EncodeToString(raw))
		if err != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
			writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
			return
		}
		s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "file_id": id, "uri": uri, "filename": header.Filename, "bytes": len(raw), "content_type": contentType, "created_at": time.Now().Unix()})
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"items": localFiles(s.cfg.ImageDataDir, "image"), "videos": localFiles(s.cfg.VideoDataDir, "video")})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, "id is required", "invalid_request_error")
			return
		}
		removed := deleteLocalMedia(s.cfg.ImageDataDir, id) + deleteLocalMedia(s.cfg.VideoDataDir, id)
		if removed == 0 {
			writeError(w, http.StatusNotFound, "file not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) grokOAuth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/grok/oauth/")
	switch {
	case path == "accounts" && r.Method == http.MethodGet:
		items, err := s.oauthStore.PublicList()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"provider": "xai_cli_oauth", "items": items, "total": len(items)})
	case path == "accounts/import" && r.Method == http.MethodPost:
		var request struct {
			AccessToken  string   `json:"access_token"`
			RefreshToken string   `json:"refresh_token"`
			IDToken      string   `json:"id_token"`
			Email        string   `json:"email"`
			Subject      string   `json:"subject"`
			ExpiresIn    int      `json:"expires_in"`
			Models       []string `json:"models"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		item, err := s.oauthStore.Import(oauth.Account{Email: request.Email, Subject: request.Subject, AccessToken: request.AccessToken, RefreshToken: request.RefreshToken, IDToken: request.IDToken, ExpiresAt: time.Now().Add(time.Duration(request.ExpiresIn) * time.Second).Unix(), Models: request.Models})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"account": oauthPublic(item)})
	case path == "device/start" && r.Method == http.MethodPost:
		var request struct {
			Proxy string `json:"proxy"`
		}
		if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &request) {
			return
		}
		result, err := s.deviceOAuth.Start(r.Context(), request.Proxy)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, result)
	case path == "device/poll" && r.Method == http.MethodPost:
		var request struct {
			ID string `json:"id"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		result, err := s.deviceOAuth.Poll(r.Context(), request.ID)
		if err != nil {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "expired") || strings.Contains(lower, "does not exist") {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			} else {
				writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
			}
			return
		}
		writeJSON(w, http.StatusOK, result)
	case path == "accounts/status" && r.Method == http.MethodPost:
		var request struct {
			IDs      []string `json:"ids"`
			Disabled bool     `json:"disabled"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		count, err := s.oauthStore.SetDisabled(request.IDs, request.Disabled)
		if err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"updated": count, "disabled": request.Disabled})
	case path == "accounts" && r.Method == http.MethodDelete:
		var request struct {
			IDs []string `json:"ids"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		count, err := s.oauthStore.Delete(request.IDs)
		if err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"removed": count})
	case strings.HasPrefix(path, "accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == http.MethodPost:
		s.handleOAuthAccountAction(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "accounts/"), "/refresh"), "refresh")
	case strings.HasPrefix(path, "accounts/") && strings.HasSuffix(path, "/models/sync") && r.Method == http.MethodPost:
		s.handleOAuthAccountAction(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "accounts/"), "/models/sync"), "models")
	case strings.HasPrefix(path, "accounts/") && strings.HasSuffix(path, "/test") && r.Method == http.MethodPost:
		s.handleOAuthAccountAction(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "accounts/"), "/test"), "test")
	default:
		writeError(w, http.StatusNotFound, "OAuth endpoint not found", "not_found")
	}
}

func (s *Server) handleOAuthAccountAction(w http.ResponseWriter, r *http.Request, id, action string) {
	items, err := s.oauthStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	var account *oauth.Account
	for index := range items {
		if items[index].ID == strings.TrimSpace(id) {
			account = &items[index]
			break
		}
	}
	if account == nil {
		writeError(w, http.StatusNotFound, "OAuth account not found", "not_found")
		return
	}
	model, prompt := "", ""
	if action == "test" {
		var request struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		model, prompt = request.Model, request.Prompt
	}
	result := s.xaiProbe.Test(r.Context(), map[string]any{
		"access_token": account.AccessToken, "refresh_token": account.RefreshToken, "id_token": account.IDToken,
	}, model, prompt)
	probe := map[string]any{"status": result.Status, "http_status": result.HTTPStatus, "code": result.Code, "quota": result.Quota, "checked_at": time.Now().UTC()}
	updated, found, updateErr := s.oauthStore.UpdateProbe(account.ID, result.AccessToken, result.RefreshToken, result.IDToken, probe, result.Error)
	if updateErr != nil {
		writeError(w, http.StatusInternalServerError, updateErr.Error(), "server_error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "OAuth account not found", "not_found")
		return
	}
	if action == "models" && result.Status == "valid" {
		updated.Models = []string{provider.XAICLIModel}
		if saved, saveErr := s.oauthStore.Import(updated); saveErr == nil {
			updated = saved
		}
	}
	status := http.StatusOK
	if result.Status == "invalid" {
		status = http.StatusUnauthorized
	} else if result.Status == "unknown" {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"ok": result.Status == "valid" || result.Status == "limited", "account": oauthPublic(updated), "probe": probe, "error": result.Error})
}

func (s *Server) grokOAuthLegacy(w http.ResponseWriter, r *http.Request) {
	clone := r.Clone(r.Context())
	urlCopy := *r.URL
	clone.URL = &urlCopy
	switch {
	case r.URL.Path == "/accounts":
		clone.URL.Path = "/api/grok/oauth/accounts"
	case strings.HasPrefix(r.URL.Path, "/accounts/"):
		clone.URL.Path = "/api/grok/oauth/accounts/" + strings.TrimPrefix(r.URL.Path, "/accounts/")
	case strings.HasPrefix(r.URL.Path, "/device/"):
		clone.URL.Path = "/api/grok/oauth/device/" + strings.TrimPrefix(r.URL.Path, "/device/")
	default:
		writeError(w, http.StatusNotFound, "OAuth endpoint not found", "not_found")
		return
	}
	s.grokOAuth(w, clone)
}

func (s *Server) grokOAuthProtocolLegacy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/protocol/")
	switch {
	case path == "start" && r.Method == http.MethodPost:
		var request struct {
			AccountID string `json:"account_id"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if strings.TrimSpace(request.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "account_id is required", "invalid_request_error")
			return
		}
		task := s.taskQueue.Submit("grok_oauth_authorize", map[string]any{"account_id": request.AccountID})
		writeJSON(w, http.StatusAccepted, map[string]any{"job": task, "job_id": task.ID})
	case path == "status" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"queue": map[string]any{"items": s.taskQueue.List(), "runtime": "go"}})
	case strings.HasPrefix(path, "jobs/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "jobs/")
		task, ok := s.taskQueue.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "protocol job not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": task})
	default:
		writeError(w, http.StatusNotFound, "protocol endpoint not found", "not_found")
	}
}

func (s *Server) taskAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks")
	if path == "" || path == "/" {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"items": s.taskQueue.List()})
			return
		}
		if r.Method == http.MethodPost {
			var request struct {
				Kind    string         `json:"kind"`
				Payload map[string]any `json:"payload"`
			}
			if !decodeJSON(w, r, &request) {
				return
			}
			if strings.TrimSpace(request.Kind) == "" {
				writeError(w, 400, "kind is required", "invalid_request_error")
				return
			}
			writeJSON(w, 202, s.taskQueue.Submit(request.Kind, request.Payload))
			return
		}
	}
	id := strings.Trim(path, "/")
	if strings.HasSuffix(id, "/cancel") && r.Method == http.MethodPost {
		id = strings.TrimSuffix(id, "/cancel")
		if !s.taskQueue.Cancel(id) {
			writeError(w, 404, "task not found or already finished", "not_found")
			return
		}
		writeJSON(w, 200, map[string]any{"cancelled": true, "id": id})
		return
	}
	if r.Method == http.MethodGet {
		task, ok := s.taskQueue.Get(id)
		if !ok {
			writeError(w, 404, "task not found", "not_found")
			return
		}
		writeJSON(w, 200, task)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
}

func (s *Server) proxyProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, _ := s.store.Config()
	profiles := mapList(cfg["proxy_profiles"])
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"profiles": profiles})
	case http.MethodPost:
		var request map[string]any
		if !decodeJSON(w, r, &request) {
			return
		}
		id := slugID(stringValue(request["id"]))
		if id == "" {
			id = fmt.Sprintf("proxy-%d", time.Now().UnixNano())
		}
		request["id"] = id
		next := make([]map[string]any, 0, len(profiles))
		replaced := false
		for _, item := range profiles {
			if stringValue(item["id"]) == id {
				next = append(next, request)
				replaced = true
			} else {
				next = append(next, item)
			}
		}
		if !replaced {
			next = append(next, request)
		}
		updated, err := s.store.UpdateConfig("proxy_profiles", next)
		if err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"profile": request, "profiles": mapList(updated["proxy_profiles"])})
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) proxyGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, _ := s.store.Config()
	groups := mapList(cfg["proxy_groups"])
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"groups": groups})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var request map[string]any
	if !decodeJSON(w, r, &request) {
		return
	}
	id := slugID(stringValue(request["id"]))
	if id == "" {
		id = fmt.Sprintf("group-%d", time.Now().UnixNano())
	}
	request["id"] = id
	next := make([]map[string]any, 0, len(groups))
	found := false
	for _, item := range groups {
		if stringValue(item["id"]) == id {
			next = append(next, request)
			found = true
		} else {
			next = append(next, item)
		}
	}
	if !found {
		next = append(next, request)
	}
	updated, err := s.store.UpdateConfig("proxy_groups", next)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	if err := s.refreshProxyRuntime(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"group": request, "groups": mapList(updated["proxy_groups"])})
}

func (s *Server) proxyHealth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "runtime": "go", "checked_at": time.Now().UTC(), "proxy": s.proxyManager.Snapshot()})
}

func localFiles(root, kind string) []map[string]any {
	entries, _ := os.ReadDir(root)
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result = append(result, map[string]any{"id": strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())), "name": entry.Name(), "kind": kind, "bytes": info.Size(), "updated_at": info.ModTime()})
	}
	return result
}
func deleteLocalMedia(root, id string) int {
	entries, _ := os.ReadDir(root)
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), id+".") {
			if os.Remove(filepath.Join(root, entry.Name())) == nil {
				count++
			}
		}
	}
	return count
}
func oauthPublic(item oauth.Account) map[string]any {
	return map[string]any{"id": item.ID, "email": item.Email, "subject": item.Subject, "status": item.Status, "source_type": item.SourceType, "has_access_token": item.AccessToken != "", "has_refresh_token": item.RefreshToken != "", "models": item.Models, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}
