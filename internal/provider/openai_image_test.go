package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOpenAIImageUploadRetriesWithStableProxyAndNeverDirect(t *testing.T) {
	payload := []byte("replayable-image-payload")
	attempts := []string{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, payload) {
			t.Fatalf("upload body changed between retries: %q", raw)
		}
		proxyURL := proxyruntime.URLFromContext(request.Context())
		attempts = append(attempts, proxyURL)
		if proxyURL == "" {
			t.Fatal("upload retry used direct egress")
		}
		if proxyURL == "http://current.invalid:8080" {
			return nil, errors.New("read: connection reset by peer")
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	manager := proxyruntime.NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []proxyruntime.GroupConfig{{ID: "images", Enabled: true, Nodes: []proxyruntime.NodeConfig{
		{ID: "current", URL: "http://current.invalid:8080", Enabled: true},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, RuntimeSuccesses: 3},
	}}})
	imageClient := NewOpenAIImage("https://chatgpt.invalid", client, manager, 10*time.Second)
	ctx := proxyruntime.WithURL(context.Background(), "http://current.invalid:8080")
	err := imageClient.uploadInputBlob(ctx, accounts.Account{}, "https://storage.invalid/upload", payload, map[string]string{"Content-Type": "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0] != "http://current.invalid:8080" || attempts[1] != "http://stable.invalid:8080" {
		t.Fatalf("unexpected proxy attempts: %#v", attempts)
	}
}

func TestOpenAIImageUploadDoesNotRetryNonRetryableResponse(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("invalid upload")), Request: request}, nil
	})}
	imageClient := NewOpenAIImage("https://chatgpt.invalid", client, nil, 10*time.Second)
	err := imageClient.uploadInputBlob(proxyruntime.WithURL(context.Background(), "http://proxy.invalid:8080"), accounts.Account{}, "https://storage.invalid/upload", []byte("image"), nil)
	if err == nil || attempts != 1 {
		t.Fatalf("expected one non-retryable attempt, attempts=%d err=%v", attempts, err)
	}
}

func TestOpenAIImageUploadWithoutStableProxyDoesNotRepeatBadRoute(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return nil, context.DeadlineExceeded
	})}
	manager := proxyruntime.NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []proxyruntime.GroupConfig{{ID: "images", Enabled: true, Nodes: []proxyruntime.NodeConfig{
		{ID: "current", URL: "http://current.invalid:8080", Enabled: true},
	}}})
	imageClient := NewOpenAIImage("https://chatgpt.invalid", client, manager, time.Second)
	err := imageClient.uploadInputBlob(
		proxyruntime.WithURL(context.Background(), "http://current.invalid:8080"),
		accounts.Account{},
		"https://storage.invalid/upload",
		[]byte("image"),
		nil,
	)
	if err == nil || attempts != 1 {
		t.Fatalf("expected one attempt without a distinct stable route, attempts=%d err=%v", attempts, err)
	}
}

func TestOpenAIImageSuccessfulUploadDoesNotReserveUnusedStableProxy(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	manager := proxyruntime.NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []proxyruntime.GroupConfig{{ID: "images", Enabled: true, Nodes: []proxyruntime.NodeConfig{
		{ID: "current", URL: "http://current.invalid:8080", Enabled: true},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1, RuntimeSuccesses: 3},
	}}})
	imageClient := NewOpenAIImage("https://chatgpt.invalid", client, manager, time.Second)
	err := imageClient.uploadInputBlob(
		proxyruntime.WithURL(context.Background(), "http://current.invalid:8080"),
		accounts.Account{},
		"https://storage.invalid/upload",
		[]byte("image"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	stable := manager.AcquireStableImage(nil, "http://current.invalid:8080")
	if stable == nil || stable.NodeID != "stable" {
		t.Fatalf("successful primary upload leaked the unused stable lease: %#v", stable)
	}
	stable.Release(false)
}

func TestOpenAIImageUploadsReferencesConcurrentlyAndKeepsOrder(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/files":
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			defer active.Add(-1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			time.Sleep(80 * time.Millisecond)
			name := stringValue(payload["file_name"])
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "file_" + name, "upload_url": serverURL(r) + "/blob/" + name})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/blob/"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploaded"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 5*time.Second)
	pngBytes := onePixelPNG(t)
	inputs := []OpenAIImageInput{
		{Name: "first.png", MIME: "image/png", Data: pngBytes},
		{Name: "second.png", MIME: "image/png", Data: pngBytes},
		{Name: "third.png", MIME: "image/png", Data: pngBytes},
	}
	started := time.Now()
	references, err := client.uploadInputs(context.Background(), accounts.Account{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("reference uploads did not overlap: %s", elapsed)
	}
	if maximum.Load() < 2 {
		t.Fatalf("expected concurrent uploads, maximum active=%d", maximum.Load())
	}
	for index, reference := range references {
		if reference.FileName != inputs[index].Name {
			t.Fatalf("reference order changed at %d: got %q want %q", index, reference.FileName, inputs[index].Name)
		}
	}
}

func TestOpenAIImageDownloadRetriesThroughStableProxyAndCoolsPrimary(t *testing.T) {
	attempts := []string{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		proxyURL := proxyruntime.URLFromContext(request.Context())
		attempts = append(attempts, proxyURL)
		if proxyURL == "http://current.invalid:8080" {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(onePixelPNG(t))),
			Request:    request,
		}, nil
	})}
	manager := proxyruntime.NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []proxyruntime.GroupConfig{{ID: "images", Enabled: true, Nodes: []proxyruntime.NodeConfig{
		{ID: "current", URL: "http://current.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, ImageConcurrencyLimit: 1, RuntimeSuccesses: 3},
	}}})
	for request := 1; request < 20; request++ {
		warmup := manager.AcquireImage(nil)
		if warmup == nil || warmup.NodeID != "stable" {
			t.Fatalf("stable proxy was not preferred before canary request: %#v", warmup)
		}
		warmup.Release(false)
	}
	primary := manager.AcquireImage(nil)
	if primary == nil || primary.NodeID != "current" {
		t.Fatalf("expected twentieth request to validate canary proxy: %#v", primary)
	}
	ctx := proxyruntime.WithImageLease(proxyruntime.WithURL(context.Background(), primary.URL), primary)
	imageClient := NewOpenAIImage("https://example.invalid", client, manager, time.Second)
	raw, mime, err := imageClient.downloadImageRefWithRetry(ctx, accounts.Account{}, "conversation", "file_test_download")
	if err != nil || len(raw) == 0 || mime != "image/png" {
		t.Fatalf("stable download retry failed: mime=%q bytes=%d err=%v", mime, len(raw), err)
	}
	primary.Release(false)
	if len(attempts) != 2 || attempts[0] != "http://current.invalid:8080" || attempts[1] != "http://stable.invalid:8080" {
		t.Fatalf("unexpected download proxy attempts: %#v", attempts)
	}
	next := manager.AcquireImage(nil)
	defer next.Release(false)
	if next.NodeID != "stable" {
		t.Fatalf("failed primary proxy was not cooled after stable retry: %#v", next)
	}
}

func TestOpenAIImagePrepareRetriesUnexpectedEOFWithStableProxy(t *testing.T) {
	attempts := []string{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts = append(attempts, proxyruntime.URLFromContext(request.Context()))
		if len(attempts) == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"file_id":"file_test","upload_url":"https://storage.invalid/upload"}`)),
			Request:    request,
		}, nil
	})}
	manager := proxyruntime.NewManager("", nil)
	manager.ConfigureImageGroups("group:images", []proxyruntime.GroupConfig{{ID: "images", Enabled: true, Nodes: []proxyruntime.NodeConfig{
		{ID: "current", URL: "http://current.invalid:8080", Enabled: true},
		{ID: "stable", URL: "http://stable.invalid:8080", Enabled: true, RuntimeSuccesses: 3},
	}}})
	imageClient := NewOpenAIImage("https://example.invalid", client, manager, time.Second)
	meta, err := imageClient.prepareInputUpload(
		proxyruntime.WithURL(context.Background(), "http://current.invalid:8080"),
		accounts.Account{},
		map[string]any{"file_name": "image.png", "file_size": 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(meta["file_id"]) != "file_test" {
		t.Fatalf("unexpected prepare metadata: %#v", meta)
	}
	if len(attempts) != 2 || attempts[0] != "http://current.invalid:8080" || attempts[1] != "http://stable.invalid:8080" {
		t.Fatalf("unexpected prepare proxy attempts: %#v", attempts)
	}
}

func TestOpenAIImagePollingTimeoutIncludesLastTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	imageClient := NewOpenAIImage("https://example.invalid", client, nil, 20*time.Millisecond)
	_, err := imageClient.pollConversation(context.Background(), accounts.Account{}, "conversation-timeout")
	if err == nil || !strings.Contains(err.Error(), "last poll error: unexpected EOF") {
		t.Fatalf("expected diagnostic polling timeout, got %v", err)
	}
}

func TestOpenAIImageTerminalErrorRecognizesUpstreamModeration(t *testing.T) {
	err := openAIImageTerminalError(map[string]any{
		"mapping": map[string]any{
			"tool": map[string]any{"message": map[string]any{"status": "blocked"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected an explicit upstream terminal error, got %v", err)
	}
	var upstream *protocol.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusUnprocessableEntity {
		t.Fatalf("unexpected terminal status: %#v", upstream)
	}
}

func TestOpenAIImageProxyFailureExcludesProviderServerErrors(t *testing.T) {
	if openAIImageProxyFailure(&protocol.UpstreamError{Status: http.StatusInternalServerError, Message: "provider unavailable"}) {
		t.Fatal("provider HTTP 500 was attributed to the proxy")
	}
	if !openAIImageProxyFailure(errors.New("read: connection reset by peer")) {
		t.Fatal("transport reset was not attributed to the proxy")
	}
}

func TestOpenAIImageGenerationFlow(t *testing.T) {
	fileID := "file_000000001234567890abcdef12345678"
	pngBytes := onePixelPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" && r.Header.Get("Authorization") != "Bearer jwt.header.payload" {
			t.Fatalf("missing OpenAI authorization on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-1\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + fileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "一只猫", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Base64 == "" || results[0].MIME != "image/png" {
		t.Fatalf("unexpected image result: %#v", results)
	}
}

func TestOpenAIImageDownloadFileRetriesPreferredRouteWhenURLIsPending(t *testing.T) {
	fileID := "file_000000001234567890abcdef12345678"
	pngBytes := onePixelPNG(t)
	var preferredAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/files/download/" + fileID:
			wantTargetRoute := "/backend-api/files/download/" + fileID
			if got := r.Header.Get("X-OpenAI-Target-Path"); got != wantTargetRoute {
				t.Fatalf("unexpected target path: %q", got)
			}
			if got := r.Header.Get("X-OpenAI-Target-Route"); got != wantTargetRoute {
				t.Fatalf("unexpected target route: %q", got)
			}
			if got := r.URL.Query().Get("post_id"); got != "" {
				t.Fatalf("unexpected post_id query value: %q", got)
			}
			if got := r.URL.Query().Get("inline"); got != "false" {
				t.Fatalf("unexpected inline query value: %q", got)
			}
			if preferredAttempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/backend-api/files/" + fileID + "/download":
			t.Fatal("legacy file download route should not be needed when the preferred route becomes ready")
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	raw, mime, err := client.downloadFile(context.Background(), accounts.Account{}, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if preferredAttempts.Load() != 2 {
		t.Fatalf("preferred route attempts = %d, want 2", preferredAttempts.Load())
	}
	if !bytes.Equal(raw, pngBytes) || mime != "image/png" {
		t.Fatalf("unexpected image download: mime=%q bytes=%d", mime, len(raw))
	}
}

func TestOpenAIImageDownloadFileFallsBackToLegacyRouteWhenPreferredRouteIsUnavailable(t *testing.T) {
	fileID := "file_000000009876543210fedcba98765432"
	pngBytes := onePixelPNG(t)
	var preferredAttempts atomic.Int32
	var legacyAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/files/download/" + fileID:
			preferredAttempts.Add(1)
			http.NotFound(w, r)
		case "/backend-api/files/" + fileID + "/download":
			legacyAttempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	raw, mime, err := client.downloadFile(context.Background(), accounts.Account{}, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if preferredAttempts.Load() != 1 || legacyAttempts.Load() != 1 {
		t.Fatalf("unexpected route attempts: preferred=%d legacy=%d", preferredAttempts.Load(), legacyAttempts.Load())
	}
	if !bytes.Equal(raw, pngBytes) || mime != "image/png" {
		t.Fatalf("unexpected image download: mime=%q bytes=%d", mime, len(raw))
	}
}

func TestOpenAIImageGenerationNeverDownloadsUserAttachment(t *testing.T) {
	inputFileID := "file_00000000aaaaaaaaaaaaaaaaaaaaaaaa"
	generatedFileID := "file_00000000bbbbbbbbbbbbbbbbbbbbbbbb"
	pngBytes := onePixelPNG(t)
	var inputDownloadAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" && r.Header.Get("Authorization") != "Bearer jwt.header.payload" {
			t.Fatalf("missing OpenAI authorization on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-source-filter\",\"message\":{\"author\":{\"role\":\"user\"},\"content\":{\"content_type\":\"multimodal_text\",\"parts\":[{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"file-service://" + inputFileID + "\"}]}}}\n\n"))
			_, _ = w.Write([]byte("data: {\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"content_type\":\"multimodal_text\",\"parts\":[{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"file-service://" + generatedFileID + "\"}]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + inputFileID + "/download":
			inputDownloadAttempts.Add(1)
			http.Error(w, "input image must not be downloaded as output", http.StatusInternalServerError)
		case "/backend-api/files/" + generatedFileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "编辑图片", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inputDownloadAttempts.Load() != 0 {
		t.Fatalf("downloaded user attachment %d time(s)", inputDownloadAttempts.Load())
	}
	if len(results) != 1 || results[0].Base64 != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("expected only the generated image, got %#v", results)
	}
}

func TestOpenAIImageGenerationFlowConversationIDVariant(t *testing.T) {
	fileID := "file_000000009876543210fedcba98765432"
	pngBytes := onePixelPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" && r.Header.Get("Authorization") != "Bearer jwt.header.payload" {
			t.Fatalf("missing OpenAI authorization on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversationId\":\"conversation-2\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + fileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "一只猫", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Base64 == "" || results[0].MIME != "image/png" {
		t.Fatalf("unexpected image result: %#v", results)
	}
}

func TestOpenAIImageGenerationPollsAndDownloadsSedimentAttachment(t *testing.T) {
	inputAttachmentID := "01JINPUTSEDIMENT12345"
	attachmentID := "01JSEDIMENT1234567890"
	pngBytes := onePixelPNG(t)
	var inputDownloadAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" && r.Header.Get("Authorization") != "Bearer jwt.header.payload" {
			t.Fatalf("missing OpenAI authorization on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-sediment\"}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-sediment":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mapping": map[string]any{
					"user-message": map[string]any{"message": map[string]any{
						"author":  map[string]any{"role": "user"},
						"content": map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + inputAttachmentID}}},
					}},
					"tool-message": map[string]any{"message": map[string]any{
						"author":   map[string]any{"role": "tool"},
						"metadata": map[string]any{"async_task_type": "image_gen"},
						"content":  map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + attachmentID}}},
					}},
				},
			})
		case "/backend-api/conversation/conversation-sediment/attachment/" + inputAttachmentID + "/download":
			inputDownloadAttempts.Add(1)
			http.Error(w, "user attachment must not be downloaded as output", http.StatusInternalServerError)
		case "/backend-api/conversation/conversation-sediment/attachment/" + attachmentID + "/download":
			if got := r.Header.Get("X-OpenAI-Target-Route"); got != "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download" {
				t.Fatalf("unexpected attachment target route: %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "一只猫", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inputDownloadAttempts.Load() != 0 {
		t.Fatalf("downloaded user attachment %d time(s)", inputDownloadAttempts.Load())
	}
	if len(results) != 1 || results[0].Base64 == "" || results[0].MIME != "image/png" {
		t.Fatalf("unexpected sediment image result: %#v", results)
	}
}

func TestOpenAIImageGenerationKeepsDownloadableResultWhenAnotherFileHasNoURL(t *testing.T) {
	goodFileID := "file_000000001234567890abcdef12345678"
	staleFileID := "file_000000009999999999abcdef12345678"
	pngBytes := onePixelPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-mixed\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"parts\":[\"file-service://" + goodFileID + "\",\"file-service://" + staleFileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + goodFileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/blob"})
		case "/backend-api/files/" + staleFileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "一只猫", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Base64 == "" {
		t.Fatalf("expected one downloadable image, got %#v", results)
	}
}

func TestOpenAIImageGenerationFailsWhenEveryFileHasNoURL(t *testing.T) {
	fileID := "file_000000009999999999abcdef12345678"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-stale\",\"message\":{\"author\":{\"role\":\"tool\"},\"metadata\":{\"async_task_type\":\"image_gen\"},\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/files/" + fileID + "/download":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	_, err := client.Generate(context.Background(), account, "一只猫", "gpt-image-2", "1024x1024", "auto", nil)
	if err == nil || !strings.Contains(err.Error(), "download returned no URL") {
		t.Fatalf("expected no URL error, got %v", err)
	}
}

func TestNormalizeOpenAIImageSize(t *testing.T) {
	tests := map[string]string{
		"1024x1365": "1024x1360",
		"1365x1024": "1360x1024",
		"1920x1080": "1920x1088",
		"1024x1536": "1024x1536",
	}
	for input, expected := range tests {
		if actual := NormalizeOpenAIImageSize(input); actual != expected {
			t.Fatalf("NormalizeOpenAIImageSize(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestOpenAIImageResolvePersistsB64JSONResponse(t *testing.T) {
	imageDir := t.TempDir()
	raw := onePixelPNG(t)
	client := NewOpenAIImage("https://example.com", nil, nil, time.Second)
	response, localURL, err := client.Resolve(
		context.Background(),
		accounts.Account{},
		ImageResult{Base64: base64.StdEncoding.EncodeToString(raw), MIME: "image/png"},
		"b64_json",
		imageDir,
		"https://images.example.com",
	)
	if err != nil {
		t.Fatal(err)
	}
	if response["b64_json"] == "" || response["url"] != "" {
		t.Fatalf("unexpected downstream response: %#v", response)
	}
	if !strings.HasPrefix(localURL, "https://images.example.com/v1/files/image?id=") {
		t.Fatalf("unexpected local URL: %q", localURL)
	}
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one persisted image, got %d", len(entries))
	}
	persisted, err := os.ReadFile(filepath.Join(imageDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted, raw) {
		t.Fatal("persisted image differs from upstream image bytes")
	}
}

func TestCollectOpenAIImageRefsAcceptsCurrentFileIDShape(t *testing.T) {
	conversationID := ""
	fileIDs := []string{}
	collectOpenAIImageRefs(map[string]any{
		"conversation_id": "conversation-current",
		"message": map[string]any{
			"author":   map[string]any{"role": "tool"},
			"metadata": map[string]any{"async_task_type": "image_gen"},
			"content": map[string]any{
				"parts": []any{map[string]any{"file_id": "file_ab12cd34ef56"}},
			},
		},
	}, &conversationID, &fileIDs)
	fileIDs = uniqueStrings(fileIDs)
	if conversationID != "conversation-current" || len(fileIDs) != 1 || fileIDs[0] != "file_ab12cd34ef56" {
		t.Fatalf("unexpected current image reference parsing: conversation=%q files=%#v", conversationID, fileIDs)
	}
}

func TestCollectOpenAIImageRefsAcceptsAssistantOutputReferenceForms(t *testing.T) {
	tests := []struct {
		name     string
		part     any
		expected string
	}{
		{
			name:     "asset pointer",
			part:     map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://file_assistantpointer123"},
			expected: "file_assistantpointer123",
		},
		{
			name:     "file service string",
			part:     "file-service://file_assistantstring123",
			expected: "file_assistantstring123",
		},
		{
			name:     "sediment string",
			part:     "sediment://01JASSISTANTSEDIMENT123",
			expected: "sediment://01JASSISTANTSEDIMENT123",
		},
		{
			name:     "file id",
			part:     map[string]any{"file_id": "file_assistantfileid123"},
			expected: "file_assistantfileid123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversationID := ""
			imageRefs := []string{}
			collectOpenAIImageRefs(map[string]any{
				"message": map[string]any{
					"author":  map[string]any{"role": "assistant"},
					"content": map[string]any{"parts": []any{tt.part}},
				},
			}, &conversationID, &imageRefs)
			if imageRefs = uniqueStrings(imageRefs); len(imageRefs) != 1 || imageRefs[0] != tt.expected {
				t.Fatalf("unexpected assistant image references: %#v", imageRefs)
			}
		})
	}
}

func TestCollectOpenAIImageRefsExcludesUserAttachments(t *testing.T) {
	conversationID := ""
	imageRefs := []string{}
	inputID := "01JINPUT1234567890"
	generatedID := "01JGENERATED123456"
	collectOpenAIImageRefs(map[string]any{
		"conversation_id": "conversation-source-filter",
		"message": map[string]any{
			"author": map[string]any{"role": "user"},
			"content": map[string]any{"parts": []any{
				map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + inputID},
			}},
		},
	}, &conversationID, &imageRefs)
	collectOpenAIImageRefs(map[string]any{
		"message": map[string]any{
			"author":   map[string]any{"role": "tool"},
			"metadata": map[string]any{"async_task_type": "image_gen"},
			"content": map[string]any{"parts": []any{
				map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + generatedID},
			}},
		},
	}, &conversationID, &imageRefs)

	imageRefs = uniqueStrings(imageRefs)
	if conversationID != "conversation-source-filter" {
		t.Fatalf("unexpected conversation id: %q", conversationID)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "sediment://"+generatedID {
		t.Fatalf("user input was accepted as a generated image: %#v", imageRefs)
	}
}

func TestCollectOpenAIImageRefsRejectsUntrustedMessageReferences(t *testing.T) {
	tests := []struct {
		name    string
		message map[string]any
	}{
		{
			name: "user image asset pointer",
			message: map[string]any{
				"author":  map[string]any{"role": "user"},
				"content": map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://file_userupload123"}}},
			},
		},
		{
			name: "unknown role file service string",
			message: map[string]any{
				"author":  map[string]any{"role": "system"},
				"content": map[string]any{"parts": []any{"file-service://file_unknownrole123"}},
			},
		},
		{
			name: "missing author sediment string",
			message: map[string]any{
				"content": map[string]any{"parts": []any{"sediment://01JUNKNOWNSEDIMENT123"}},
			},
		},
		{
			name: "malformed author file id",
			message: map[string]any{
				"author":  "assistant",
				"content": map[string]any{"parts": []any{map[string]any{"file_id": "file_malformedauthor123"}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversationID := ""
			imageRefs := []string{}
			collectOpenAIImageRefs(map[string]any{"message": tt.message}, &conversationID, &imageRefs)
			if len(imageRefs) != 0 {
				t.Fatalf("untrusted message yielded image references: %#v", imageRefs)
			}
		})
	}
}

func TestCollectOpenAIImageRefsAcceptsSedimentPointer(t *testing.T) {
	conversationID := ""
	imageRefs := []string{}
	collectOpenAIImageRefs(map[string]any{
		"conversation_id": "conversation-sediment",
		"message": map[string]any{"author": map[string]any{"role": "tool"}, "metadata": map[string]any{"async_task_type": "image_gen"}, "content": map[string]any{"parts": []any{
			map[string]any{"asset_pointer": "sediment://01JSEDIMENT1234567890"},
		}}},
	}, &conversationID, &imageRefs)
	imageRefs = uniqueStrings(imageRefs)
	if conversationID != "conversation-sediment" || len(imageRefs) != 1 || imageRefs[0] != "sediment://01JSEDIMENT1234567890" {
		t.Fatalf("unexpected sediment parsing: conversation=%q refs=%#v", conversationID, imageRefs)
	}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 1, 1))
	canvas.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func serverURL(r *http.Request) string {
	value := "http://" + r.Host
	if strings.TrimSpace(r.Host) == "" {
		return "http://127.0.0.1"
	}
	return value
}
