package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestValidOpenAIImageSizeAcceptsArbitraryDimensions(t *testing.T) {
	for _, value := range []string{"864x1152", "1800x2400", "123x456"} {
		if !validOpenAIImageSize(value) {
			t.Fatalf("rejected %s", value)
		}
	}
	for _, value := range []string{"0x100", "abc", "100"} {
		if validOpenAIImageSize(value) {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestImageEditsAlwaysReturnServerURL(t *testing.T) {
	for _, requested := range []string{"", "url", "b64_json"} {
		if actual := imageEditResponseFormat(requested); actual != "url" {
			t.Fatalf("imageEditResponseFormat(%q) = %q, want url", requested, actual)
		}
	}
}

var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestParseImageEditRequestAcceptsJSONDataURL(t *testing.T) {
	server := &Server{}
	body := `{"model":"grok-imagine-image-edit","prompt":"换成夜景","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(tinyPNG) + `","n":1}`
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	parsed, err := server.parseImageEditRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt != "换成夜景" || parsed.Model != "grok-imagine-image-edit" || len(parsed.Inputs) != 1 {
		t.Fatalf("unexpected parsed request: %#v", parsed)
	}
	if parsed.Inputs[0].MIME != "image/png" || len(parsed.Inputs[0].Data) != len(tinyPNG) {
		t.Fatalf("unexpected parsed image: %#v", parsed.Inputs[0])
	}
}

func TestParseImageEditRequestReportsTruncatedJSON(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"gpt-image-2","prompt":`))
	request.Header.Set("Content-Type", "application/json")

	_, err := server.parseImageEditRequest(request)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected a truncated JSON diagnostic, got %v", err)
	}
}

func TestParseImageEditRequestRejectsOversizedJSON(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = maxJSONBodyBytes + 1

	_, err := server.parseImageEditRequest(request)
	var parseError imageEditParseError
	if !errors.As(err, &parseError) || parseError.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected a 413 parse error, got %#v", err)
	}
}

func TestImageInputsFromChatContentIgnoresPlainPrompt(t *testing.T) {
	server := &Server{}
	inputs, err := server.imageInputsFromChatContent(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 {
		t.Fatalf("expected no image inputs for plain prompt, got %d", len(inputs))
	}
}

func TestImageInputsFromChatContentAcceptsImageBlock(t *testing.T) {
	server := &Server{}
	value := []any{map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG),
		},
	}}
	inputs, err := server.imageInputsFromChatContent(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || len(inputs[0].Data) != len(tinyPNG) {
		t.Fatalf("unexpected image inputs: %#v", inputs)
	}
}

func TestValidateRemoteImageURLRejectsPrivateAddresses(t *testing.T) {
	t.Setenv("GO_ALLOW_PRIVATE_IMAGE_URLS", "false")
	for _, source := range []string{
		"http://127.0.0.1/image.png",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.1/image.png",
		"http://localhost/image.png",
	} {
		if err := validateRemoteImageURL(context.Background(), source); err == nil {
			t.Fatalf("expected %s to be rejected", source)
		}
	}
}

func TestValidateRemoteImageURLAcceptsPublicAddress(t *testing.T) {
	t.Setenv("GO_ALLOW_PRIVATE_IMAGE_URLS", "false")
	if err := validateRemoteImageURL(context.Background(), "https://8.8.8.8/image.png"); err != nil {
		t.Fatalf("expected public address to be accepted: %v", err)
	}
}

func TestRequestPublicBaseDoesNotTrustPublicHostByDefault(t *testing.T) {
	t.Setenv("GO_PUBLIC_BASE_URL", "")
	t.Setenv("CHATGPT2API_BASE_URL", "")
	t.Setenv("GO_TRUST_REQUEST_PUBLIC_BASE", "false")
	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/v1/images/generations", nil)
	request.Host = "attacker.example"
	if value := requestPublicBase(request); value != "" {
		t.Fatalf("expected empty public base, got %q", value)
	}
	request.Host = "127.0.0.1:3000"
	if value := requestPublicBase(request); value != "http://127.0.0.1:3000" {
		t.Fatalf("expected loopback public base, got %q", value)
	}
}

func TestRequestPublicBaseUsesConfiguredPublicURL(t *testing.T) {
	t.Setenv("GO_PUBLIC_BASE_URL", "http://23.148.212.231:8000/")
	t.Setenv("CHATGPT2API_BASE_URL", "")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3000/v1/images/generations", nil)
	if value := requestPublicBase(request); value != "http://23.148.212.231:8000" {
		t.Fatalf("expected configured public base, got %q", value)
	}
}

func TestUpstreamStatusUnwrapsWrappedErrors(t *testing.T) {
	err := fmt.Errorf("do request failed /v1/chat/completions: %w", &protocol.UpstreamError{Status: http.StatusTooManyRequests, Message: "throttled"})
	if status := upstreamStatus(err); status != http.StatusTooManyRequests {
		t.Fatalf("expected wrapped upstream status 429, got %d", status)
	}
}

func TestUpstreamStatusMapsDeadlineToGatewayTimeout(t *testing.T) {
	err := fmt.Errorf("image polling failed: %w", context.DeadlineExceeded)
	if status := upstreamStatus(err); status != http.StatusGatewayTimeout {
		t.Fatalf("expected deadline status 504, got %d", status)
	}
}

func TestEditableFileDownloadRequiresValidSignature(t *testing.T) {
	root := t.TempDir()
	fileRoot := filepath.Join(root, "files")
	if err := os.MkdirAll(fileRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileRoot, "result.zip"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{DataDir: root, APIKey: "api-secret"}}
	unsigned := httptest.NewRequest(http.MethodGet, "/files/result.zip", nil)
	unsignedResponse := httptest.NewRecorder()
	server.downloadEditableFile(unsignedResponse, unsigned)
	if unsignedResponse.Code != http.StatusNotFound {
		t.Fatalf("expected unsigned download to be rejected, got %d", unsignedResponse.Code)
	}
	signature := editableFileSignature("api-secret", "result.zip")
	signed := httptest.NewRequest(http.MethodGet, "/files/result.zip?signature="+signature, nil)
	signedResponse := httptest.NewRecorder()
	server.downloadEditableFile(signedResponse, signed)
	if signedResponse.Code != http.StatusOK || signedResponse.Body.String() != "payload" {
		t.Fatalf("unexpected signed download response: %d %q", signedResponse.Code, signedResponse.Body.String())
	}
}

func TestParseImageEditRequestAcceptsMultipartImageAliases(t *testing.T) {
	server := &Server{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "grok-imagine-image-edit")
	_ = writer.WriteField("prompt", "加一点玻璃反光")
	part, err := writer.CreateFormFile("images[]", "source.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(tinyPNG); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())

	parsed, err := server.parseImageEditRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt != "加一点玻璃反光" || len(parsed.Inputs) != 1 || parsed.Inputs[0].Name != "source.png" {
		t.Fatalf("unexpected parsed request: %#v", parsed)
	}
}

func TestParseImageEditRequestReportsMissingMultipartBoundary(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader("--broken"))
	request.Header.Set("Content-Type", "multipart/form-data")

	_, err := server.parseImageEditRequest(request)
	if err == nil || !strings.Contains(err.Error(), "missing boundary") {
		t.Fatalf("expected missing boundary error, got %v", err)
	}
}

func TestImageGenerationsRunsOpenAIBatchesConcurrently(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.ConfigPath = root + "\\config.json"
	cfg.AccountsPath = root + "\\accounts.json"
	cfg.AuthKeysPath = root + "\\auth_keys.json"
	cfg.ImageDataDir = root + "\\images"

	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{
		{"access_token": "jwt.header.payload", "email": "a@example.test", "pool": "basic"},
		{"access_token": "jwt.header.payload2", "email": "b@example.test", "pool": "basic"},
	}); err != nil {
		t.Fatal(err)
	}

	var inflight atomic.Int32
	var peak atomic.Int32
	fileID := "file_000000001234567890abcdef12345678"
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" && !strings.HasPrefix(r.URL.Path, "/blob/") && r.Header.Get("Authorization") == "" {
			t.Fatalf("missing OpenAI authorization on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html data-build="test-build"><script src="/static/app.js"></script></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"prepare_token": "prepare-token"})
		case "/backend-api/sentinel/chat-requirements/finalize":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "requirements-token"})
		case "/backend-api/f/conversation/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"conduit_token": "conduit-token"})
		case "/backend-api/f/conversation":
			current := inflight.Add(1)
			for {
				prev := peak.Load()
				if current <= prev {
					break
				}
				if peak.CompareAndSwap(prev, current) {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-1\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"content_type\":\"multimodal_text\",\"parts\":[{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"file-service://" + fileID + "\"}]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			inflight.Add(-1)
		case "/backend-api/files/" + fileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": upstream.URL + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(tinyPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, 30*time.Second)

	requestBody := `{"model":"gpt-image-2","prompt":"一只猫","n":2,"size":"1024x1024","response_format":"url"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data) != 2 {
		t.Fatalf("expected 2 images, got %d", len(decoded.Data))
	}
	if peak.Load() < 2 {
		t.Fatalf("expected concurrent OpenAI image requests, peak=%d", peak.Load())
	}
}

func TestOpenAIImageRequestsPersistOnlyReturnedImageCount(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")
	cfg.ImageDataDir = filepath.Join(root, "images")

	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{
		{"access_token": "jwt.header.payload", "email": "a@example.test", "pool": "basic"},
	}); err != nil {
		t.Fatal(err)
	}

	fileIDs := []string{
		"file_000000001234567890abcdef12345678",
		"file_000000001234567890abcdef87654321",
	}
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html data-build="test-build"><script src="/static/app.js"></script></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"prepare_token": "prepare-token"})
		case "/backend-api/sentinel/chat-requirements/finalize":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "requirements-token"})
		case "/backend-api/f/conversation/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"conduit_token": "conduit-token"})
		case "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"conversation_id\":\"conversation-1\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"content_type\":\"multimodal_text\",\"parts\":[{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"file-service://%s\"},{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"file-service://%s\"}]}}}\n\n", fileIDs[0], fileIDs[1])
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + fileIDs[0] + "/download", "/backend-api/files/" + fileIDs[1] + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": upstream.URL + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(tinyPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	server.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, 30*time.Second)

	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"一只猫","n":1,"response_format":"url"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data) != 1 {
		t.Fatalf("expected 1 returned image, got %d", len(decoded.Data))
	}
	entries, err := os.ReadDir(cfg.ImageDataDir)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".meta.json") {
			stored++
		}
	}
	if stored != 1 {
		t.Fatalf("expected 1 persisted image, got %d", stored)
	}

	chatRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-image-2","messages":[{"role":"user","content":"一只猫"}]}`))
	chatRequest.Header.Set("Content-Type", "application/json")
	chatRequest.Header.Set("Authorization", "Bearer api-secret")
	chatResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusOK {
		t.Fatalf("unexpected chat response: %d %s", chatResponse.Code, chatResponse.Body.String())
	}
	if count := strings.Count(chatResponse.Body.String(), "![image]("); count != 1 {
		t.Fatalf("expected 1 returned chat image, got %d: %s", count, chatResponse.Body.String())
	}
	entries, err = os.ReadDir(cfg.ImageDataDir)
	if err != nil {
		t.Fatal(err)
	}
	stored = 0
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".meta.json") {
			stored++
		}
	}
	if stored != 2 {
		t.Fatalf("expected one persisted image per request, got %d total", stored)
	}
}
