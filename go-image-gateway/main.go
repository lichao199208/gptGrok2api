package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const queueKey = "image-gateway:queue"

type config struct {
	ListenAddr     string
	RedisAddr      string
	BackendURL     string
	SchedulerURL   string
	SchedulerKey   string
	MonitorURL     string
	LogURL         string
	Workers        int
	QueueCapacity  int64
	BackendTimeout time.Duration
	TaskTTL        time.Duration
	MaxBodyBytes   int64
	AuthKeysFile   string
	AdminKey       string
	MaxAttempts    int
}

type task struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Authorization string          `json:"authorization,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
	Attempts      int             `json:"attempts"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type editFile struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Data string `json:"data"`
}

type publicTask struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Attempts  int             `json:"attempts"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type reservation struct {
	ID           string `json:"reservation_id"`
	AccountEmail string `json:"account_email"`
	ProxySource  string `json:"proxy_source"`
	ProxyGroupID string `json:"proxy_group_id"`
	ProxyNodeID  string `json:"proxy_node_id"`
}

type server struct {
	cfg       config
	rdb       *redis.Client
	client    *http.Client
	started   time.Time
	accepted  atomic.Uint64
	completed atomic.Uint64
	failed    atomic.Uint64
	active    atomic.Int64
	wg        sync.WaitGroup
	waiters   sync.Map
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func loadConfig() config {
	return config{
		ListenAddr:     env("IMAGE_GATEWAY_LISTEN", ":8080"),
		RedisAddr:      env("IMAGE_GATEWAY_REDIS_ADDR", "redis:6379"),
		BackendURL:     strings.TrimRight(env("IMAGE_GATEWAY_BACKEND_URL", "http://app/v1/images/generations"), "/"),
		SchedulerURL:   strings.TrimRight(env("IMAGE_GATEWAY_SCHEDULER_URL", "http://app/internal/image-scheduler"), "/"),
		SchedulerKey:   strings.TrimSpace(os.Getenv("IMAGE_GATEWAY_SCHEDULER_KEY")),
		MonitorURL:     strings.TrimRight(env("IMAGE_GATEWAY_MONITOR_URL", "http://app/internal/image-monitor"), "/"),
		LogURL:         strings.TrimRight(env("IMAGE_GATEWAY_LOG_URL", "http://app/internal/logs/call"), "/"),
		Workers:        envInt("IMAGE_GATEWAY_WORKERS", 500),
		QueueCapacity:  int64(envInt("IMAGE_GATEWAY_QUEUE_CAPACITY", 10000)),
		BackendTimeout: time.Duration(envInt("IMAGE_GATEWAY_BACKEND_TIMEOUT_SECS", 900)) * time.Second,
		TaskTTL:        time.Duration(envInt("IMAGE_GATEWAY_TASK_TTL_SECS", 86400)) * time.Second,
		MaxBodyBytes:   int64(envInt("IMAGE_GATEWAY_MAX_BODY_MB", 2)) << 20,
		AuthKeysFile:   env("IMAGE_GATEWAY_AUTH_KEYS_FILE", "/etc/image-gateway/auth_keys.json"),
		AdminKey:       strings.TrimSpace(os.Getenv("IMAGE_GATEWAY_AUTH_KEY")),
		MaxAttempts:    envInt("IMAGE_GATEWAY_MAX_ATTEMPTS", 1),
	}
}

func (s *server) authorized(header string) bool {
	token := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" {
		return false
	}
	if s.cfg.AdminKey != "" && token == s.cfg.AdminKey {
		return true
	}
	raw, err := os.ReadFile(filepath.Clean(s.cfg.AuthKeysFile))
	if err != nil {
		return false
	}
	var doc struct {
		Items []struct {
			KeyHash string `json:"key_hash"`
			Enabled bool   `json:"enabled"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	for _, item := range doc.Items {
		if item.Enabled && item.KeyHash == hash {
			return true
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func taskKey(id string) string { return "image-gateway:task:" + id }

func (s *server) saveTask(ctx context.Context, item task) error {
	item.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(item)
	ttl := s.cfg.TaskTTL
	if item.Status == "success" || item.Status == "failed" {
		ttl = 5 * time.Minute
	}
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, taskKey(item.ID), raw, ttl).Err()
}

func (s *server) loadTask(ctx context.Context, id string) (task, error) {
	var item task
	raw, err := s.rdb.Get(ctx, taskKey(id)).Bytes()
	if err != nil {
		return item, err
	}
	err = json.Unmarshal(raw, &item)
	return item, err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.authorized(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	var check map[string]any
	if json.Unmarshal(raw, &check) != nil || strings.TrimSpace(fmt.Sprint(check["prompt"])) == "" {
		writeJSON(w, 400, map[string]string{"error": "valid JSON prompt is required"})
		return
	}
	s.enqueue(w, r, raw)
	return
}

func (s *server) enqueue(w http.ResponseWriter, r *http.Request, raw []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	length, err := s.rdb.LLen(ctx, queueKey).Result()
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "queue unavailable"})
		return
	}
	if length >= s.cfg.QueueCapacity {
		writeJSON(w, 429, map[string]string{"error": "task queue is full"})
		return
	}
	now := time.Now().UTC()
	item := task{ID: newID(), Status: "queued", Payload: raw, Authorization: r.Header.Get("Authorization"), CreatedAt: now, UpdatedAt: now}
	if err := s.saveTask(ctx, item); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to persist task"})
		return
	}
	ch := make(chan task, 1)
	s.waiters.Store(item.ID, ch)
	defer s.waiters.Delete(item.ID)
	if err := s.rdb.RPush(ctx, queueKey, item.ID).Err(); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to enqueue task"})
		return
	}
	s.accepted.Add(1)
	select {
	case done := <-ch:
		if done.Status == "success" {
			writeJSON(w, http.StatusOK, json.RawMessage(done.Result))
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": done.Error, "type": "server_error"}})
	case <-r.Context().Done():
		return
	}
}

func (s *server) submitEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.authorized(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		s.submitJSONEditRaw(w, r, raw)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid multipart request"})
		return
	}
	form := r.MultipartForm
	if form == nil {
		writeJSON(w, 400, map[string]string{"error": "multipart form is required"})
		return
	}
	files := func(keys ...string) ([]editFile, error) {
		var out []editFile
		for _, key := range keys {
			for _, header := range form.File[key] {
				file, err := header.Open()
				if err != nil {
					return nil, err
				}
				data, err := io.ReadAll(io.LimitReader(file, 50<<20))
				file.Close()
				if err != nil {
					return nil, err
				}
				out = append(out, editFile{Name: header.Filename, Mime: header.Header.Get("Content-Type"), Data: base64.StdEncoding.EncodeToString(data)})
			}
		}
		return out, nil
	}
	images, err := files("image", "image[]")
	if err != nil || len(images) == 0 {
		writeJSON(w, 400, map[string]string{"error": "image file or image_url is required"})
		return
	}
	masks, err := files("mask")
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid mask"})
		return
	}
	payload := map[string]any{"_edit": true, "prompt": firstFormValue(form.Value["prompt"]), "model": firstFormValue(form.Value["model"]), "n": firstFormValue(form.Value["n"]), "size": firstFormValue(form.Value["size"]), "quality": firstFormValue(form.Value["quality"]), "response_format": firstFormValue(form.Value["response_format"]), "images": images, "mask": masks}
	if payload["model"] == "" {
		payload["model"] = "gpt-image-2"
	}
	if payload["n"] == "" {
		payload["n"] = "1"
	}
	if payload["size"] == "" {
		payload["size"] = "1024x1024"
	}
	if payload["response_format"] == "" {
		payload["response_format"] = "b64_json"
	}
	raw, _ = json.Marshal(payload)
	s.enqueue(w, r, raw)
	return
}

func (s *server) submitJSONEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	s.submitJSONEditRaw(w, r, raw)
}

func (s *server) submitJSONEditRaw(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON request"})
		return
	}
	if mask := input["mask"]; len(mask) > 0 && string(mask) != "null" && string(mask) != `""` {
		writeJSON(w, 400, map[string]string{"error": "mask is not supported yet"})
		return
	}
	values := []json.RawMessage{}
	for _, key := range []string{"image", "images", "images[]", "image_url", "image_url_parts"} {
		raw := input[key]
		if len(raw) == 0 {
			continue
		}
		var many []json.RawMessage
		if json.Unmarshal(raw, &many) == nil {
			values = append(values, many...)
		} else {
			values = append(values, raw)
		}
	}
	if len(values) == 0 {
		writeJSON(w, 400, map[string]string{"error": "image is required"})
		return
	}
	images := []editFile{}
	for i, value := range values {
		var dataURL string
		if json.Unmarshal(value, &dataURL) != nil {
			writeJSON(w, 400, map[string]string{"error": "image must be a string"})
			return
		}
		if strings.HasPrefix(strings.ToLower(dataURL), "http://") || strings.HasPrefix(strings.ToLower(dataURL), "https://") {
			resp, fetchErr := http.Get(dataURL)
			if fetchErr != nil || resp.StatusCode >= 400 {
				if resp != nil {
					resp.Body.Close()
				}
				writeJSON(w, 400, map[string]string{"error": "invalid image URL"})
				return
			}
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 50<<20+1))
			resp.Body.Close()
			if readErr != nil || len(b) > 50<<20 {
				writeJSON(w, 400, map[string]string{"error": "invalid image URL"})
				return
			}
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "image/png"
			}
			dataURL = "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(b)
		}
		header, encoded, ok := strings.Cut(dataURL, ",")
		if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
			writeJSON(w, 400, map[string]string{"error": "image must be a base64 data URL"})
			return
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid image base64 data"})
			return
		}
		mime := "image/png"
		if semi := strings.Index(header, ";"); semi > 5 {
			mime = header[5:semi]
		}
		images = append(images, editFile{Name: fmt.Sprintf("image-%d.png", i+1), Mime: mime, Data: base64.StdEncoding.EncodeToString(data)})
	}
	get := func(name, fallback string) string {
		var value string
		if json.Unmarshal(input[name], &value) == nil && value != "" {
			return value
		}
		return fallback
	}
	n := "1"
	if value := input["n"]; len(value) > 0 {
		var number int
		if json.Unmarshal(value, &number) == nil {
			n = strconv.Itoa(number)
		} else {
			var text string
			if json.Unmarshal(value, &text) == nil && text != "" {
				n = text
			}
		}
	}
	payload := map[string]any{"_edit": true, "prompt": get("prompt", ""), "model": get("model", "gpt-image-2"), "n": n, "size": get("size", "1024x1024"), "quality": get("quality", ""), "response_format": get("response_format", "b64_json"), "images": images}
	encoded, _ := json.Marshal(payload)
	s.enqueue(w, r, encoded)
}

func firstFormValue(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/image-tasks/")
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, 404, map[string]string{"error": "task not found"})
		return
	}
	item, err := s.loadTask(r.Context(), id)
	if errors.Is(err, redis.Nil) {
		writeJSON(w, 404, map[string]string{"error": "task not found"})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "task store unavailable"})
		return
	}
	writeJSON(w, 200, publicTask{ID: item.ID, Status: item.Status, Result: item.Result, Error: item.Error, Attempts: item.Attempts, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	err := s.rdb.Ping(ctx).Err()
	queued, _ := s.rdb.LLen(ctx, queueKey).Result()
	status := 200
	state := "ok"
	if err != nil {
		status, state = 503, "degraded"
	}
	writeJSON(w, status, map[string]any{"status": state, "redis_ok": err == nil, "workers": s.cfg.Workers, "active": s.active.Load(), "queued": queued, "accepted": s.accepted.Load(), "completed": s.completed.Load(), "failed": s.failed.Load(), "uptime_secs": int(time.Since(s.started).Seconds())})
}

func (s *server) worker(ctx context.Context, workerID int) {
	defer s.wg.Done()
	for {
		result, err := s.rdb.BLPop(ctx, 5*time.Second, queueKey).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, redis.Nil) {
				log.Printf("worker=%d queue_error=%v", workerID, err)
				time.Sleep(time.Second)
			}
			continue
		}
		if len(result) != 2 {
			continue
		}
		s.runTask(ctx, result[1])
	}
}

func (s *server) runTask(parent context.Context, id string) {
	item, err := s.loadTask(parent, id)
	if err != nil {
		return
	}
	item.Status = "running"
	item.Attempts++
	_ = s.saveTask(parent, item)
	requestSummary := imageSummary(item.Payload)
	s.active.Add(1)
	defer s.active.Add(-1)
	if item.Attempts == 1 {
		s.monitorStart(item)
	}
	s.monitorStage(item.ID, "image_getting_account", map[string]any{"model": imageModel(item.Payload), "handler_queue_ms": time.Since(item.CreatedAt).Milliseconds()})
	ctx, cancel := context.WithTimeout(parent, s.cfg.BackendTimeout)
	defer cancel()
	lease, err := s.scheduler(ctx, "reserve", item.Payload)
	statusCode := 0
	if err == nil {
		var response json.RawMessage
		s.monitorStage(item.ID, "image_egress_ready", map[string]any{"model": imageModel(item.Payload)})
		s.monitorStage(item.ID, "image_starting_generation", map[string]any{"model": imageModel(item.Payload)})
		if isEditPayload(item.Payload) {
			response, statusCode, err = s.schedulerExecuteEdit(ctx, lease.ID, item.ID, item.Payload)
		} else {
			response, statusCode, err = s.schedulerExecute(ctx, lease.ID, item.ID, item.Payload)
		}
		// release is idempotent; execute also releases in its finally block.
		_ = s.schedulerRelease(context.Background(), lease.ID, false)
		if err == nil {
			item.Status, item.Result, item.Payload, item.Authorization = "success", response, nil, ""
			s.completed.Add(1)
			_ = s.saveTask(parent, item)
			s.monitorFinish(item, "success", "")
			s.logCall(item, requestSummary, "success", "", lease)
			s.notify(item)
			return
		}
	}
	retryable := err != nil && (statusCode == 0 || statusCode == http.StatusTooManyRequests || statusCode >= 500)
	if retryable && item.Attempts < s.cfg.MaxAttempts {
		item.Status = "queued"
		_ = s.saveTask(parent, item)
		delay := time.Duration(1<<(item.Attempts-1)) * time.Second
		s.monitorStage(item.ID, "image_retry_wait", map[string]any{"model": imageModel(item.Payload), "retry_wait_ms": delay.Milliseconds(), "error": truncate(err)})
		go func(taskID string, wait time.Duration) {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			<-timer.C
			_ = s.rdb.RPush(context.Background(), queueKey, taskID).Err()
		}(item.ID, delay)
		return
	}
	item.Status, item.Error, item.Payload, item.Authorization = "failed", truncate(err), nil, ""
	s.failed.Add(1)
	_ = s.saveTask(parent, item)
	s.monitorFinish(item, "failed", item.Error)
	s.logCall(item, requestSummary, "failed", item.Error, lease)
	s.notify(item)
}

func (s *server) scheduler(ctx context.Context, action string, payload []byte) (reservation, error) {
	var empty reservation
	var body []byte
	if action == "reserve" {
		body = []byte(`{"model":"gpt-image-2"}`)
	} else {
		body = payload
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/reserve", bytes.NewReader(body))
	if err != nil {
		return empty, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return empty, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || resp.StatusCode >= 300 {
		return empty, fmt.Errorf("scheduler reserve HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var value reservation
	if json.Unmarshal(raw, &value) != nil || value.ID == "" {
		return empty, errors.New("scheduler returned no reservation")
	}
	return value, nil
}

func (s *server) schedulerExecute(ctx context.Context, reservation, callID string, payload []byte) (json.RawMessage, int, error) {
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, 0, fmt.Errorf("invalid task payload: %w", err)
	}
	request["_call_id"] = callID
	request["_trace_image_perf"] = true
	// The reservation executor collects one final image response.  Normalize
	// client-side streaming requests here instead of failing them with a 400;
	// the public image endpoint remains OpenAI-compatible while the internal
	// account/proxy lease is always released deterministically.
	request["stream"] = false
	body, err := json.Marshal(map[string]any{"request": request})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("scheduler execute HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return raw, resp.StatusCode, nil
}

func imageModel(payload []byte) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(payload, &body)
	if strings.TrimSpace(body.Model) == "" {
		return "gpt-image-2"
	}
	return body.Model
}
func imageSummary(payload []byte) string {
	var body struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Prompt
}
func isEditPayload(payload []byte) bool {
	var body struct {
		Edit bool `json:"_edit"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Edit
}

func (s *server) schedulerExecuteEdit(ctx context.Context, reservation, callID string, payload []byte) (json.RawMessage, int, error) {
	var body struct {
		Prompt, Model, N, Size, Quality, ResponseFormat string
		Images                                          []editFile `json:"images"`
		Mask                                            []editFile `json:"mask"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("prompt", body.Prompt)
	_ = writer.WriteField("model", body.Model)
	_ = writer.WriteField("n", body.N)
	_ = writer.WriteField("size", body.Size)
	_ = writer.WriteField("quality", body.Quality)
	_ = writer.WriteField("response_format", body.ResponseFormat)
	_ = writer.WriteField("_call_id", callID)
	for _, item := range append(body.Images, body.Mask...) {
		data, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return nil, 0, err
		}
		field := "image[]"
		if containsFile(body.Mask, item) {
			field = "mask"
		}
		part, err := writer.CreateFormFile(field, item.Name)
		if err != nil {
			return nil, 0, err
		}
		if _, err = part.Write(data); err != nil {
			return nil, 0, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/execute-edit", &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("scheduler execute-edit HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return raw, resp.StatusCode, nil
}
func containsFile(files []editFile, target editFile) bool {
	for _, item := range files {
		if item.Data == target.Data {
			return true
		}
	}
	return false
}
func (s *server) monitor(ctx context.Context, action string, body map[string]any) {
	if s.cfg.MonitorURL == "" || s.cfg.SchedulerKey == "" {
		return
	}
	s.monitorTo(ctx, s.cfg.MonitorURL+"/"+action, body)
}
func (s *server) monitorTo(ctx context.Context, endpoint string, body map[string]any) {
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	response, err := s.client.Do(request)
	if err == nil && response != nil {
		response.Body.Close()
	}
}
func (s *server) monitorStart(item task) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "start", map[string]any{"call_id": item.ID, "endpoint": taskEndpoint(item), "model": imageModel(item.Payload), "summary": imageSummary(item.Payload)})
}
func (s *server) monitorStage(callID, event string, data map[string]any) {
	data["call_id"] = callID
	data["event"] = event
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "stage", data)
}
func (s *server) monitorFinish(item task, status, errorText string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "finish", map[string]any{"call_id": item.ID, "endpoint": taskEndpoint(item), "model": imageModel(item.Payload), "status": status, "duration_ms": time.Since(item.CreatedAt).Milliseconds(), "error": errorText})
}

func taskEndpoint(item task) string {
	var p map[string]any
	if json.Unmarshal(item.Payload, &p) == nil {
		if v, ok := p["_edit"].(bool); ok && v {
			return "/v1/images/edits"
		}
	}
	return "/v1/images/generations"
}
func (s *server) logCall(item task, summary, status, errorText string, lease reservation) {
	if s.cfg.LogURL == "" || s.cfg.SchedulerKey == "" {
		return
	}
	body := map[string]any{"summary": summary, "detail": map[string]any{"call_id": item.ID, "endpoint": "/v1/images/generations", "model": imageModel(item.Payload), "status": status, "duration_ms": time.Since(item.CreatedAt).Milliseconds(), "attempts": item.Attempts, "error": errorText, "gateway": "go-image-gateway", "account_email": lease.AccountEmail, "proxy_source": lease.ProxySource, "proxy_group_id": lease.ProxyGroupID, "proxy_node_id": lease.ProxyNodeID, "has_proxy": lease.ProxySource != "", "request_text": summary, "perf": map[string]any{"total_ms": time.Since(item.CreatedAt).Milliseconds()}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitorTo(ctx, s.cfg.LogURL, body)
}

func (s *server) schedulerRelease(ctx context.Context, reservation string, failed bool) error {
	body := []byte(`{"failed":false}`)
	if failed {
		body = []byte(`{"failed":true}`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/release", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	resp, err := s.client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

func (s *server) notify(item task) {
	if value, ok := s.waiters.Load(item.ID); ok {
		value.(chan task) <- item
	}
}

func truncate(err error) string {
	if err == nil {
		return "unknown backend error"
	}
	value := err.Error()
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("chatgpt2api-image-gateway reservation-sync 1.0")
		return
	}
	cfg := loadConfig()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, PoolSize: max(32, cfg.Workers+16), MinIdleConns: 8})
	s := &server{cfg: cfg, rdb: rdb, started: time.Now(), client: &http.Client{Transport: &http.Transport{MaxIdleConns: cfg.Workers * 2, MaxIdleConnsPerHost: cfg.Workers, MaxConnsPerHost: cfg.Workers, IdleConnTimeout: 90 * time.Second}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis unavailable: %v", err)
	}
	for i := 0; i < cfg.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, i)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/images/generations", s.submit)
	mux.HandleFunc("/v1/images/edits", s.submitEdit)
	mux.HandleFunc("/v1/image-tasks/", s.status)
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("image gateway listening=%s workers=%d queue_capacity=%d", cfg.ListenAddr, cfg.Workers, cfg.QueueCapacity)
	log.Fatal(httpServer.ListenAndServe())
}
