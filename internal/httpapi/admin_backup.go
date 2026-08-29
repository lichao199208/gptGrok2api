package httpapi

import (
	"archive/zip"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) backupsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/backups")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		s.listBackups(w)
	case path == "/run" && r.Method == http.MethodPost:
		s.runBackup(w)
	case path == "/delete" && r.Method == http.MethodPost:
		s.deleteBackup(w, r)
	case path == "/detail" && r.Method == http.MethodGet:
		s.backupDetail(w, r)
	case path == "/download" && r.Method == http.MethodGet:
		s.downloadBackup(w, r)
	default:
		writeError(w, http.StatusNotFound, "backup endpoint not found", "not_found")
	}
}

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

func (s *Server) listBackups(w http.ResponseWriter) {
	entries, _ := os.ReadDir(s.backupDir())
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zip" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, backupItem(entry.Name(), info))
	}
	sort.Slice(items, func(i, j int) bool { return stringValue(items[i]["created_at"]) > stringValue(items[j]["created_at"]) })
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"state":    map[string]any{"running": false, "last_error": ""},
		"settings": map[string]any{"backend": "local", "directory": s.backupDir()},
	})
}

func (s *Server) runBackup(w http.ResponseWriter) {
	if err := os.MkdirAll(s.backupDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	name := fmt.Sprintf("backup-%s.zip", time.Now().UTC().Format("20060102-150405"))
	path := filepath.Join(s.backupDir(), name)
	file, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	archive := zip.NewWriter(file)
	files := []struct {
		path string
		name string
	}{
		{s.cfg.AccountsPath, "data/accounts.json"},
		{s.cfg.AuthKeysPath, "data/auth_keys.json"},
		{s.cfg.ConfigPath, "config.json"},
		{s.cfg.OAuthPath, "data/oauth_accounts.json.enc"},
		{s.cfg.QueuePath, "data/tasks.json"},
		{s.tagsPath(), "data/image_tags.json"},
	}
	for _, item := range files {
		if err := addBackupFile(archive, item.path, item.name); err != nil {
			_ = archive.Close()
			_ = file.Close()
			_ = os.Remove(path)
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	if err := archive.Close(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	info, _ := os.Stat(path)
	writeJSON(w, http.StatusOK, map[string]any{"result": backupItem(name, info), "ok": true})
}

func addBackupFile(archive *zip.Writer, path, name string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	writer, err := archive.Create(name)
	if err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func backupItem(name string, info os.FileInfo) map[string]any {
	item := map[string]any{"key": name, "name": name, "type": "local", "content_type": "application/zip"}
	if info != nil {
		item["size"] = info.Size()
		item["created_at"] = info.ModTime().UTC()
		item["updated_at"] = info.ModTime().UTC()
	}
	return item
}

func (s *Server) backupPath(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" || filepath.Base(key) != key || filepath.Ext(key) != ".zip" {
		return "", false
	}
	path := filepath.Join(s.backupDir(), key)
	if !isWithin(s.backupDir(), path) {
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	path, ok := s.backupPath(request.Key)
	if !ok || os.Remove(path) != nil {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) backupDetail(w http.ResponseWriter, r *http.Request) {
	path, ok := s.backupPath(r.URL.Query().Get("key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": backupItem(info.Name(), info)})
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	path, ok := s.backupPath(r.URL.Query().Get("key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}
