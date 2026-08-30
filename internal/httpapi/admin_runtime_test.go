package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func adminTestConfig(root string) config.Config {
	return config.Config{
		RootDir: root, DataDir: filepath.Join(root, "data"), StaticDir: filepath.Join(root, "web_dist"),
		ConfigPath: filepath.Join(root, "config.json"), AccountsPath: filepath.Join(root, "data", "accounts.json"),
		AuthKeysPath: filepath.Join(root, "data", "auth_keys.json"), APIKey: "api-secret", AdminKey: "admin-secret", Version: "test",
		ImageDataDir: filepath.Join(root, "data", "files", "images"), VideoDataDir: filepath.Join(root, "data", "files", "videos"),
		OAuthPath: filepath.Join(root, "data", "oauth.json.enc"), QueuePath: filepath.Join(root, "data", "tasks.json"),
		RegisterPath: filepath.Join(root, "data", "register.json"), GrokAccountsPath: filepath.Join(root, "data", "grok_accounts.json"),
	}
}

func adminRequest(handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer admin-secret")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRuntimeMonitorLifecycle(t *testing.T) {
	monitor := newRuntimeMonitor()
	monitor.start("call-1", "/v1/videos", "video", "hello")
	monitor.update("call-1", "in_progress", 50, "")
	item, ok := monitor.detail("call-1")
	if !ok || item.Progress != 50 || item.Status != "running" {
		t.Fatalf("unexpected active item: %#v %v", item, ok)
	}
	monitor.finish("call-1", "success", "", "", "")
	item, ok = monitor.detail("call-1")
	if !ok || item.Status != "success" || item.Progress != 100 || item.Duration < 0 {
		t.Fatalf("unexpected completed item: %#v %v", item, ok)
	}
}

func TestRequestMonitorEnrichmentUpdatesLiveEgressAndAccount(t *testing.T) {
	server := &Server{monitor: newRuntimeMonitor()}
	server.monitor.start("call-egress", "/v1/images/generations", "gpt-image-2", "test")
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	request = request.WithContext(context.WithValue(request.Context(), monitorCallIDKey{}, "call-egress"))

	server.enrichMonitorAccount(request, accounts.Account{Pool: "basic", Fields: map[string]any{
		"email":     "image@example.test",
		"proxy_url": "http://proxy-user:proxy-pass@203.0.113.8:8080",
	}})
	server.enrichRequestMonitor(request, map[string]any{
		"egress_label": "http://203.0.113.8:8080",
		"has_proxy":    true,
	})

	record, ok := server.monitor.detail("call-egress")
	if !ok {
		t.Fatal("active monitor record missing")
	}
	if record.AccountEmail != "image@example.test" {
		t.Fatalf("unexpected account email: %q", record.AccountEmail)
	}
	if record.ProxySource != "account" || record.EgressLabel != "http://203.0.113.8:8080" || !record.HasProxy {
		t.Fatalf("unexpected egress metadata: %#v", record)
	}
	if strings.Contains(record.EgressLabel, "proxy-user") || strings.Contains(record.EgressLabel, "proxy-pass") {
		t.Fatalf("proxy credentials leaked into monitor label: %q", record.EgressLabel)
	}
}

func TestMonitorSnapshotWithHistorySummary(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	history := map[string]any{
		"type":    "call",
		"id":      "call-2",
		"summary": "history prompt",
		"detail": map[string]any{
			"call_id":     "call-2",
			"endpoint":    "/v1/images/generations",
			"model":       "gpt-image-2",
			"status":      "success",
			"started_at":  now.Add(-2 * time.Second).Format(time.RFC3339),
			"ended_at":    now.Format(time.RFC3339),
			"duration_ms": 2000,
			"monitor": map[string]any{
				"stage": "download",
				"metrics": map[string]any{
					"handler_queue_ms":      100,
					"stream_first_queue_ms": 120,
					"account_wait_ms":       140,
					"egress_wait_ms":        160,
					"download_ms":           180,
					"total_ms":              3000,
				},
				"perf": map[string]any{
					"response_ms": 220,
				},
				"events": []map[string]any{
					{"time": now.Format(time.RFC3339), "event": "download", "label": "下载", "download_ms": 180},
				},
			},
		},
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "logs.jsonl"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	server := &Server{cfg: cfg, monitor: newRuntimeMonitor()}
	server.monitor.start("call-1", "/v1/chat/completions", "gpt-4o", "hello")
	server.monitor.update("call-1", "running", 35, "")

	snapshot := server.monitorSnapshotWithHistory()
	summary, ok := snapshot["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing: %#v", snapshot["summary"])
	}
	if got := monitorNumber(summary["active"]); got != 1 {
		t.Fatalf("unexpected active count: %v", got)
	}
	if got := monitorNumber(summary["completed"]); got != 1 {
		t.Fatalf("unexpected completed count: %v", got)
	}
	if got := monitorNumber(summary["p95_duration_ms"]); got <= 0 {
		t.Fatalf("p95 duration missing: %#v", summary)
	}
	metricP95, ok := summary["metric_p95"].(map[string]any)
	if !ok || monitorNumber(metricP95["handler_queue_ms"]) <= 0 || monitorNumber(metricP95["total_ms"]) <= 0 {
		t.Fatalf("metric p95 missing: %#v", summary["metric_p95"])
	}
	bottleneck, ok := summary["bottleneck"].(map[string]any)
	if !ok || stringValue(bottleneck["label"]) == "" || monitorNumber(bottleneck["value_ms"]) <= 0 {
		t.Fatalf("bottleneck missing: %#v", summary["bottleneck"])
	}
	activeByModel, ok := summary["active_by_model"].(map[string]any)
	if !ok || monitorNumber(activeByModel["gpt-4o"]) != 1 {
		t.Fatalf("active_by_model missing: %#v", summary["active_by_model"])
	}
	activeByStage, ok := summary["active_by_stage"].(map[string]any)
	if !ok || monitorNumber(activeByStage["running"]) != 1 {
		t.Fatalf("active_by_stage missing: %#v", summary["active_by_stage"])
	}
}

func TestRequestMonitorWritesMultipartCallLog(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := &Server{cfg: cfg, monitor: newRuntimeMonitor()}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "make a blue square"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.withRequestMonitor(response, request, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("unexpected log line count: %d", len(lines))
	}
	var item map[string]any
	if err := json.Unmarshal(lines[0], &item); err != nil {
		t.Fatal(err)
	}
	detail := mapValue(item["detail"])
	if stringValue(detail["model"]) != "gpt-image-2" {
		t.Fatalf("model not logged: %#v", detail)
	}
	if !strings.Contains(stringValue(detail["request_text"]), "blue square") {
		t.Fatalf("prompt not logged: %#v", detail)
	}
	if stringValue(mapValue(detail["request_shape"])["content_type"]) != "multipart/form-data" {
		t.Fatalf("request shape not logged: %#v", detail)
	}
	monitor := mapValue(detail["monitor"])
	if len(anyList(monitor["events"])) < 2 {
		t.Fatalf("monitor events missing: %#v", detail)
	}
}

func TestResponseImageOutputsIgnoresMalformedURLs(t *testing.T) {
	raw := []byte(`{"choices":[{"message":{"content":"![image](http://%zz/v1/files/image?id=bad)"}}]}`)
	outputs := responseImageOutputs(raw)
	if len(outputs) != 0 {
		t.Fatalf("malformed image URL should be ignored: %#v", outputs)
	}

	raw = []byte(`{"choices":[{"message":{"content":"![image](http://127.0.0.1:8000/v1/files/image?id=ok)"}}]}`)
	outputs = responseImageOutputs(raw)
	if len(outputs) != 1 || outputs[0]["filename"] != "ok" {
		t.Fatalf("valid image URL was not recorded: %#v", outputs)
	}
}

func TestCleanupExpiredImagesUsesRetentionDays(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	cfg.ImageRetentionDays = 1
	server := &Server{cfg: cfg}
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(cfg.ImageDataDir, "old.png")
	newPath := filepath.Join(cfg.ImageDataDir, "new.png")
	metaPath := filepath.Join(cfg.ImageDataDir, "old.png.meta.json")
	for _, path := range []string{oldPath, newPath, metaPath} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metaPath, old, old); err != nil {
		t.Fatal(err)
	}
	removed, bytes := server.cleanupExpiredImages()
	if removed != 2 || bytes != 8 {
		t.Fatalf("unexpected cleanup result: removed=%d bytes=%d", removed, bytes)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old image was not removed: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("old metadata was not removed: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new image should remain: %v", err)
	}
}

func TestAdminImagesTagsAndBackup(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ImageDataDir, "image-one.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	list := adminRequest(handler, http.MethodGet, "/api/images", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "image-one.png") {
		t.Fatalf("unexpected image list: %d %s", list.Code, list.Body.String())
	}

	tagsBody := strings.NewReader(`{"path":"image-one.png","tags":["work","work"]}`)
	tags := adminRequest(handler, http.MethodPost, "/api/images/tags", tagsBody)
	if tags.Code != http.StatusOK || !strings.Contains(tags.Body.String(), "work") {
		t.Fatalf("unexpected tags response: %d %s", tags.Code, tags.Body.String())
	}
	allTags := adminRequest(handler, http.MethodGet, "/api/images/tags", nil)
	if allTags.Code != http.StatusOK || !strings.Contains(allTags.Body.String(), "work") {
		t.Fatalf("unexpected tag list: %d %s", allTags.Code, allTags.Body.String())
	}

	backup := adminRequest(handler, http.MethodPost, "/api/backups/run", nil)
	if backup.Code != http.StatusOK {
		t.Fatalf("backup failed: %d %s", backup.Code, backup.Body.String())
	}
	var backupResponse map[string]any
	if err := json.Unmarshal(backup.Body.Bytes(), &backupResponse); err != nil {
		t.Fatal(err)
	}
	result, _ := backupResponse["result"].(map[string]any)
	key, _ := result["key"].(string)
	if key == "" {
		t.Fatalf("backup key missing: %#v", backupResponse)
	}
	archivePath := filepath.Join(cfg.DataDir, "backups", key)
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()

	delete := adminRequest(handler, http.MethodPost, "/api/images/delete", strings.NewReader(`{"paths":["image-one.png"]}`))
	if delete.Code != http.StatusOK || strings.Contains(delete.Body.String(), `"removed":0`) {
		t.Fatalf("image deletion failed: %d %s", delete.Code, delete.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.ImageDataDir, "image-one.png")); !os.IsNotExist(err) {
		t.Fatalf("image was not deleted: %v", err)
	}
}

func TestRegistrationManagementEndpoints(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(filepath.Dir(cfg.GrokAccountsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	account := `[{"id":"grok-one","email":"alice@example.com","password":"secret","sso":"sso-token","status":"active","source_type":"protocol"}]`
	if err := os.WriteFile(cfg.GrokAccountsPath, []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/register", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("register endpoint should require admin key: %d", unauthorized.Code)
	}

	configResponse := adminRequest(handler, http.MethodGet, "/api/register", nil)
	if configResponse.Code != http.StatusOK || !strings.Contains(configResponse.Body.String(), `"register"`) {
		t.Fatalf("unexpected register config: %d %s", configResponse.Code, configResponse.Body.String())
	}
	startResponse := adminRequest(handler, http.MethodPost, "/api/register/start", nil)
	if startResponse.Code != http.StatusServiceUnavailable || !strings.Contains(startResponse.Body.String(), `"ready":false`) {
		t.Fatalf("register start should report unavailable executor: %d %s", startResponse.Code, startResponse.Body.String())
	}
	runtimeResponse := adminRequest(handler, http.MethodGet, "/api/register/runtime", nil)
	if runtimeResponse.Code != http.StatusOK || !strings.Contains(runtimeResponse.Body.String(), `"ready":false`) {
		t.Fatalf("unexpected registration runtime status: %d %s", runtimeResponse.Code, runtimeResponse.Body.String())
	}
	listResponse := adminRequest(handler, http.MethodGet, "/api/register/grok/accounts?page_size=10", nil)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "al***e@example.com") {
		t.Fatalf("register list failed: %d %s", listResponse.Code, listResponse.Body.String())
	}
	if strings.Contains(listResponse.Body.String(), "sso-token") || strings.Contains(listResponse.Body.String(), `"password":"secret"`) {
		t.Fatalf("register list leaked credentials: %s", listResponse.Body.String())
	}

	authorizeResponse := adminRequest(handler, http.MethodPost, "/api/register/grok/accounts/oauth/authorize", strings.NewReader(`{"ids":["grok-one"]}`))
	if authorizeResponse.Code != http.StatusOK || !strings.Contains(authorizeResponse.Body.String(), `"status":"queued"`) {
		t.Fatalf("OAuth authorization was not queued: %d %s", authorizeResponse.Code, authorizeResponse.Body.String())
	}
	credentialsResponse := adminRequest(handler, http.MethodGet, "/api/register/grok/accounts/grok-one/credentials", nil)
	if credentialsResponse.Code != http.StatusOK || !strings.Contains(credentialsResponse.Body.String(), "secret") {
		t.Fatalf("credentials endpoint failed: %d %s", credentialsResponse.Code, credentialsResponse.Body.String())
	}
	ssoResponse := adminRequest(handler, http.MethodGet, "/api/register/grok/accounts/export-sso", nil)
	if ssoResponse.Code != http.StatusOK || !strings.Contains(ssoResponse.Body.String(), "sso-token") {
		t.Fatalf("SSO export failed: %d %s", ssoResponse.Code, ssoResponse.Body.String())
	}
	disableResponse := adminRequest(handler, http.MethodPost, "/api/register/grok/accounts/runtime/disabled", strings.NewReader(`{"ids":["grok-one"],"disabled":true}`))
	if disableResponse.Code != http.StatusOK || !strings.Contains(disableResponse.Body.String(), `"ok":1`) {
		t.Fatalf("disable endpoint failed: %d %s", disableResponse.Code, disableResponse.Body.String())
	}
}

func TestImportedAbnormalAccountCleanup(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(filepath.Dir(cfg.AccountsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	accounts := `[
  {"access_token":"abnormal-token","status":"异常","enabled":true},
  {"access_token":"normal-token","status":"正常","enabled":true}
]`
	if err := os.WriteFile(cfg.AccountsPath, []byte(accounts), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	preview := adminRequest(handler, http.MethodPost, "/api/accounts/import-cleanup", strings.NewReader(`{"access_tokens":["abnormal-token","normal-token","abnormal-token"],"remove":false}`))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"checked":2`) || !strings.Contains(preview.Body.String(), `"abnormal":1`) || !strings.Contains(preview.Body.String(), `"removed":0`) {
		t.Fatalf("unexpected cleanup preview: %d %s", preview.Code, preview.Body.String())
	}

	removed := adminRequest(handler, http.MethodPost, "/api/accounts/import-cleanup", strings.NewReader(`{"access_tokens":["abnormal-token","normal-token"],"remove":true}`))
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"abnormal":1`) || !strings.Contains(removed.Body.String(), `"removed":1`) {
		t.Fatalf("unexpected cleanup result: %d %s", removed.Code, removed.Body.String())
	}
	items, err := New(cfg).store.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || stringValue(items[0]["access_token"]) != "normal-token" {
		t.Fatalf("unexpected accounts after cleanup: %#v", items)
	}
}
