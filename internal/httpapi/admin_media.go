package httpapi

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var imageTagsMu sync.Mutex

func (s *Server) adminImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/images")
	switch {
	case path == "" || path == "/":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		s.listAdminImages(w, r)
	case path == "/tags" && r.Method == http.MethodGet:
		s.listImageTags(w)
	case path == "/tags" && r.Method == http.MethodPost:
		s.updateImageTags(w, r)
	case strings.HasPrefix(path, "/tags/") && r.Method == http.MethodDelete:
		tag := strings.TrimPrefix(path, "/tags/")
		s.deleteImageTag(w, tag)
	case path == "/storage" && r.Method == http.MethodGet:
		s.imageStorage(w)
	case path == "/storage/compress" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "compressed": 0, "saved_bytes": 0, "message": "Go runtime does not recompress source media"})
	case path == "/storage/cleanup-to-target" && r.Method == http.MethodPost:
		s.imageStorageCleanup(w, r)
	case path == "/delete" && r.Method == http.MethodPost:
		s.deleteAdminImages(w, r)
	case path == "/download" && r.Method == http.MethodPost:
		s.downloadAdminImages(w, r)
	case strings.HasPrefix(path, "/download/") && r.Method == http.MethodGet:
		s.downloadSingleImage(w, r, strings.TrimPrefix(path, "/download/"))
	default:
		writeError(w, http.StatusNotFound, "image endpoint not found", "not_found")
	}
}

func (s *Server) listAdminImages(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0)
	items = append(items, listMediaItems(s.cfg.ImageDataDir, "image", "")...)
	items = append(items, listMediaItems(s.cfg.VideoDataDir, "video", "video/")...)
	tags := s.loadImageTags()
	query := r.URL.Query()
	mediaType := strings.ToLower(strings.TrimSpace(query.Get("media_type")))
	tag := strings.TrimSpace(query.Get("tag"))
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		itemTags := stringList(tags[stringValue(item["path"])])
		item["tags"] = itemTags
		if mediaType != "" && mediaType != "all" && stringValue(item["type"]) != mediaType {
			continue
		}
		if tag != "" && tag != "all" && !containsString(itemTags, tag) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(fmt.Sprint(item["path"], " ", item["name"], " ", item["updated_at"])), search) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return stringValue(filtered[i]["updated_at"]) > stringValue(filtered[j]["updated_at"])
	})
	total := len(filtered)
	offset := nonNegativeInt(query.Get("offset"), 0)
	limit := nonNegativeInt(query.Get("limit"), 0)
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	page := filtered[offset:end]
	counts := map[string]int{"all": total, "image": 0, "video": 0}
	for _, item := range filtered {
		counts[stringValue(item["type"])]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "groups": []any{}, "total": total, "total_size": mediaItemsSize(filtered), "counts": counts, "limit": limit, "offset": offset, "has_more": end < total})
}

func listMediaItems(root, kind, prefix string) []map[string]any {
	entries, _ := os.ReadDir(root)
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !isWithin(root, path) {
			continue
		}
		rel := prefix + filepath.ToSlash(entry.Name())
		value := map[string]any{"path": rel, "rel": rel, "name": entry.Name(), "filename": entry.Name(), "type": kind, "size": info.Size(), "bytes": info.Size(), "updated_at": info.ModTime().UTC(), "url": "/images/" + rel}
		if meta := mediaMetadata(path); meta != nil {
			for key, item := range meta {
				value[key] = item
			}
		} else if kind == "image" {
			value["source_type"] = "legacy_output"
			value["role"] = "output"
			value["generated_at"] = info.ModTime().UTC().Format(time.RFC3339)
		}
		items = append(items, value)
	}
	return items
}

func mediaItemsSize(items []map[string]any) int64 {
	var total int64
	for _, item := range items {
		if value, ok := item["size"].(int64); ok {
			total += value
		}
	}
	return total
}

func (s *Server) publicImage(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/images/")
	root := s.cfg.ImageDataDir
	if strings.HasPrefix(r.URL.Path, "/image-thumbnails/") {
		path = strings.TrimPrefix(r.URL.Path, "/image-thumbnails/")
	}
	if path == "" || strings.Contains(path, "\\") {
		writeError(w, http.StatusBadRequest, "invalid image path", "invalid_request_error")
		return
	}
	path, err := filepath.Rel(root, filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	filePath := filepath.Join(root, path)
	if _, err := os.Stat(filePath); err != nil {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(filePath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, filePath)
}

func (s *Server) deleteAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths       []string `json:"paths"`
		AllMatching bool     `json:"all_matching"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	paths := request.Paths
	if request.AllMatching {
		paths = nil
		for _, item := range listMediaItems(s.cfg.ImageDataDir, "image", "") {
			paths = append(paths, stringValue(item["path"]))
		}
	}
	removed := 0
	for _, item := range paths {
		if s.removeMediaPath(item) {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) removeMediaPath(value string) bool {
	root := s.cfg.ImageDataDir
	value = filepath.ToSlash(strings.TrimSpace(value))
	if strings.HasPrefix(value, "video/") {
		root = s.cfg.VideoDataDir
		value = strings.TrimPrefix(value, "video/")
	}
	path := filepath.Join(root, filepath.FromSlash(value))
	if !isWithin(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return false
	}
	if os.Remove(path) == nil {
		_ = os.Remove(path + ".meta.json")
		s.removeImageTags(value)
		return true
	}
	return false
}

func (s *Server) removeImageTags(path string) {
	all := s.loadImageTags()
	if _, ok := all[path]; !ok {
		return
	}
	delete(all, path)
	_ = s.saveImageTags(all)
}

func (s *Server) downloadAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="images.zip"`)
	if err := s.writeMediaZip(w, request.Paths); err != nil {
		return
	}
}

func (s *Server) downloadSingleImage(w http.ResponseWriter, r *http.Request, path string) {
	root, clean := s.mediaPath(path)
	if root == "" {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(clean)+`"`)
	http.ServeFile(w, r, filepath.Join(root, clean))
}

func (s *Server) writeMediaZip(w io.Writer, paths []string) error {
	archive := zip.NewWriter(w)
	defer archive.Close()
	used := map[string]bool{}
	added := 0
	for _, item := range paths {
		root, clean := s.mediaPath(item)
		if root == "" {
			continue
		}
		path := filepath.Join(root, clean)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(clean)
		if used[name] {
			name = fmt.Sprintf("%d_%s", added+1, name)
		}
		used[name] = true
		writer, err := archive.Create(name)
		if err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		added++
	}
	if added == 0 {
		return fmt.Errorf("no media found")
	}
	return nil
}

func (s *Server) mediaPath(value string) (string, string) {
	value = filepath.ToSlash(strings.Trim(strings.TrimSpace(value), "/"))
	root := s.cfg.ImageDataDir
	if strings.HasPrefix(value, "video/") {
		root = s.cfg.VideoDataDir
		value = strings.TrimPrefix(value, "video/")
	}
	if value == "" || strings.Contains(value, "\\") {
		return "", ""
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	path := filepath.Join(root, clean)
	if !isWithin(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return "", ""
	}
	if _, err := os.Stat(path); err != nil {
		return "", ""
	}
	return root, clean
}

func (s *Server) tagsPath() string { return filepath.Join(s.cfg.DataDir, "image_tags.json") }

func (s *Server) loadImageTags() map[string]any {
	imageTagsMu.Lock()
	defer imageTagsMu.Unlock()
	raw, err := os.ReadFile(s.tagsPath())
	if err != nil {
		return map[string]any{}
	}
	var tags map[string]any
	if json.Unmarshal(raw, &tags) != nil {
		return map[string]any{}
	}
	return tags
}

func (s *Server) saveImageTags(tags map[string]any) error {
	imageTagsMu.Lock()
	defer imageTagsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.tagsPath()), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tags, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.tagsPath(), append(raw, '\n'), 0o600)
}

func (s *Server) listImageTags(w http.ResponseWriter) {
	all := map[string]bool{}
	for _, value := range s.loadImageTags() {
		for _, tag := range stringList(value) {
			all[tag] = true
		}
	}
	result := make([]string, 0, len(all))
	for tag := range all {
		result = append(result, tag)
	}
	sort.Strings(result)
	writeJSON(w, http.StatusOK, map[string]any{"tags": result})
}

func (s *Server) updateImageTags(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &request) || strings.TrimSpace(request.Path) == "" {
		return
	}
	path := filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(request.Path), "/"))
	tags := map[string]any{}
	for _, tag := range request.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags[tag] = true
		}
	}
	values := make([]string, 0, len(tags))
	for tag := range tags {
		values = append(values, tag)
	}
	sort.Strings(values)
	all := s.loadImageTags()
	all[path] = values
	if err := s.saveImageTags(all); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tags": values})
}

func (s *Server) deleteImageTag(w http.ResponseWriter, tag string) {
	tag = strings.TrimSpace(tag)
	all := s.loadImageTags()
	removed := 0
	for path, value := range all {
		values := stringList(value)
		next := make([]string, 0, len(values))
		changed := false
		for _, item := range values {
			if item == tag {
				changed = true
				continue
			}
			next = append(next, item)
		}
		if changed {
			removed++
			all[path] = next
		}
	}
	if err := s.saveImageTags(all); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_from": removed})
}

func (s *Server) imageStorage(w http.ResponseWriter) {
	images := mediaStats(s.cfg.ImageDataDir)
	total, used, free := diskUsage(s.cfg.ImageDataDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"disk_total_mb":    total / (1024 * 1024),
		"disk_used_mb":     used / (1024 * 1024),
		"disk_free_mb":     free / (1024 * 1024),
		"image_count":      images["count"],
		"image_size_mb":    images["size_bytes"].(int64) / (1024 * 1024),
		"image_size_bytes": images["size_bytes"],
		"images":           images,
		"videos":           mediaStats(s.cfg.VideoDataDir),
	})
}

func (s *Server) imageStorageCleanup(w http.ResponseWriter, r *http.Request) {
	target := positiveInt(r.URL.Query().Get("target_free_mb"), 500)
	dryRun := strings.EqualFold(r.URL.Query().Get("dry_run"), "true") || r.URL.Query().Get("dry_run") == "1"
	totalBytes, _, freeBytes := diskUsage(s.cfg.ImageDataDir)
	if totalBytes > 0 && uint64(target) > totalBytes/(1024*1024) {
		writeError(w, http.StatusBadRequest, "target free space exceeds disk capacity; enter the value in MB", "invalid_request_error")
		return
	}
	currentFree := freeBytes / (1024 * 1024)
	removed, freed := 0, int64(0)
	files := imageFilesByAge(s.cfg.ImageDataDir)
	for _, file := range files {
		if currentFree+uint64(freed/(1024*1024)) >= uint64(target) {
			break
		}
		if !dryRun {
			if err := os.Remove(file.path); err != nil {
				continue
			}
			_ = os.Remove(file.path + ".meta.json")
			s.removeImageTags(file.rel)
		}
		freed += file.size
		removed++
	}
	if !dryRun {
		cleanupEmptyDirs(s.cfg.ImageDataDir)
		_, _, freeBytes = diskUsage(s.cfg.ImageDataDir)
		currentFree = freeBytes / (1024 * 1024)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "target_free_mb": target, "dry_run": dryRun,
		"removed": removed, "freed_mb": freed / (1024 * 1024),
		"current_free_mb": currentFree, "done": currentFree >= uint64(target) || (dryRun && currentFree+uint64(freed/(1024*1024)) >= uint64(target)),
	})
}

type imageStorageFile struct {
	path  string
	rel   string
	size  int64
	mtime time.Time
}

func imageFilesByAge(root string) []imageStorageFile {
	files := []imageStorageFile{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || strings.HasSuffix(info.Name(), ".meta.json") || !isImageStorageFile(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			files = append(files, imageStorageFile{path: path, rel: filepath.ToSlash(rel), size: info.Size(), mtime: info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	return files
}

func isImageStorageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	default:
		return false
	}
}

func cleanupEmptyDirs(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && info.IsDir() && path != root {
			entries, readErr := os.ReadDir(path)
			if readErr == nil && len(entries) == 0 {
				_ = os.Remove(path)
			}
		}
		return nil
	})
}

func stringList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []string:
		return append(result, typed...)
	case []any:
		for _, item := range typed {
			if value := strings.TrimSpace(fmt.Sprint(item)); value != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nonNegativeInt(value string, fallback int) int {
	var number int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &number); err != nil || number < 0 {
		return fallback
	}
	return number
}
