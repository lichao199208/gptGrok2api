package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-1\",\"message\":{\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-1":
			writeGeneratedImageConversation(w, fileID)
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
			_, _ = w.Write([]byte("data: {\"conversationId\":\"conversation-2\",\"message\":{\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-2":
			writeGeneratedImageConversation(w, fileID)
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
	attachmentID := "01JSEDIMENT1234567890"
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-sediment\"}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-sediment":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mapping": map[string]any{"tool-message": map[string]any{"message": map[string]any{
					"author":   map[string]any{"role": "tool"},
					"metadata": map[string]any{"async_task_type": "image_gen"},
					"content":  map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + attachmentID}}},
				}}},
			})
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
	if len(results) != 1 || results[0].Base64 == "" || results[0].MIME != "image/png" {
		t.Fatalf("unexpected sediment image result: %#v", results)
	}
}

func TestOpenAIImageGenerationDownloadsAssistantOutputInsteadOfEchoedReference(t *testing.T) {
	referenceID := "file_00000000111111111111111111111111"
	generatedID := "file_00000000222222222222222222222222"
	referenceBytes := onePixelPNG(t)
	generatedBytes := []byte("generated-image-output")
	var referenceDownloads atomic.Int32
	var generatedDownloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html data-build="test-build"><script src="/static/app.js"></script></html>`))
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/files":
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": referenceID, "upload_url": serverURL(r) + "/reference-upload"})
		case r.Method == http.MethodPut && r.URL.Path == "/reference-upload":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/files/"+referenceID+"/uploaded":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"prepare_token": "prepare-token"})
		case r.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "requirements-token"})
		case r.URL.Path == "/backend-api/f/conversation/prepare":
			_ = json.NewEncoder(w).Encode(map[string]any{"conduit_token": "conduit-token"})
		case r.URL.Path == "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"conversation_id\":\"conversation-reference\",\"message\":{\"author\":{\"role\":\"user\"},\"content\":{\"parts\":[{\"asset_pointer\":\"file-service://%s\"}]}}}\n\n", referenceID)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case r.URL.Path == "/backend-api/conversation/conversation-reference":
			_ = json.NewEncoder(w).Encode(map[string]any{"mapping": map[string]any{
				"user": map[string]any{"message": map[string]any{
					"author": map[string]any{"role": "user"}, "create_time": 1,
					"content": map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + referenceID}}},
				}},
				"assistant": map[string]any{"message": map[string]any{
					"author": map[string]any{"role": "assistant"}, "create_time": 2,
					"content": map[string]any{"parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + generatedID}}},
				}},
			}})
		case r.URL.Path == "/backend-api/files/"+referenceID+"/download":
			referenceDownloads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/reference-blob"})
		case r.URL.Path == "/backend-api/files/"+generatedID+"/download":
			generatedDownloads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"download_url": serverURL(r) + "/generated-blob"})
		case r.URL.Path == "/reference-blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(referenceBytes)
		case r.URL.Path == "/generated-blob":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(generatedBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenAIImage(server.URL, server.Client(), nil, 10*time.Second)
	account := accounts.Account{Token: "jwt.header.payload", Fields: map[string]any{"source_type": "chatgpt_web"}}
	results, err := client.Generate(context.Background(), account, "按参考图生成", "gpt-image-2", "1024x1024", "auto", []OpenAIImageInput{{Name: "reference.png", MIME: "image/png", Data: referenceBytes}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one generated image, got %#v", results)
	}
	raw, err := base64.StdEncoding.DecodeString(results[0].Base64)
	if err != nil || !bytes.Equal(raw, generatedBytes) {
		t.Fatalf("returned bytes are not the generated output: raw=%q err=%v", raw, err)
	}
	if referenceDownloads.Load() != 0 || generatedDownloads.Load() != 1 {
		t.Fatalf("unexpected downloads: reference=%d generated=%d", referenceDownloads.Load(), generatedDownloads.Load())
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-mixed\",\"message\":{\"content\":{\"parts\":[\"file-service://" + goodFileID + "\",\"file-service://" + staleFileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-mixed":
			writeGeneratedImageConversation(w, goodFileID, staleFileID)
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
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation-stale\",\"message\":{\"content\":{\"parts\":[\"file-service://" + fileID + "\"]}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/backend-api/conversation/conversation-stale":
			writeGeneratedImageConversation(w, fileID)
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

func TestCollectOpenAIImageRefsAcceptsSedimentPointer(t *testing.T) {
	conversationID := ""
	imageRefs := []string{}
	collectOpenAIImageRefs(map[string]any{
		"conversation_id": "conversation-sediment",
		"message": map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"asset_pointer": "sediment://01JSEDIMENT1234567890"},
		}}},
	}, &conversationID, &imageRefs)
	imageRefs = uniqueStrings(imageRefs)
	if conversationID != "conversation-sediment" || len(imageRefs) != 1 || imageRefs[0] != "sediment://01JSEDIMENT1234567890" {
		t.Fatalf("unexpected sediment parsing: conversation=%q refs=%#v", conversationID, imageRefs)
	}
}

func TestCollectOpenAIGeneratedImageRefsUsesOnlyOrderedOutputRecords(t *testing.T) {
	conversationID := ""
	refs := []string{}
	collectOpenAIGeneratedImageRefs(map[string]any{
		"conversation_id": "conversation-generated",
		"mapping": map[string]any{
			"user-message": map[string]any{"message": map[string]any{
				"author": map[string]any{"role": "user"}, "create_time": 1,
				"content": map[string]any{"asset_pointer": "sediment://file_reference"},
			}},
			"assistant-text": map[string]any{"message": map[string]any{
				"author": map[string]any{"role": "assistant"}, "create_time": 2,
				"content": map[string]any{"parts": []any{"unrelated file_00000000999999999999999999999999"}},
			}},
			"later-assistant-image": map[string]any{"message": map[string]any{
				"author": map[string]any{"role": "assistant"}, "create_time": 4,
				"content": map[string]any{"asset_pointer": "sediment://assistant_generated"},
			}},
			"earlier-tool-image": map[string]any{"message": map[string]any{
				"author": map[string]any{"role": "tool"}, "create_time": 3,
				"metadata": map[string]any{"async_task_type": "image_gen"},
				"content":  map[string]any{"asset_pointer": "sediment://tool_generated"},
			}},
		},
	}, &conversationID, &refs)
	if conversationID != "conversation-generated" || len(refs) != 2 || refs[0] != "sediment://tool_generated" || refs[1] != "sediment://assistant_generated" {
		t.Fatalf("unexpected generated refs: conversation=%q refs=%#v", conversationID, refs)
	}
}

func writeGeneratedImageConversation(w http.ResponseWriter, fileIDs ...string) {
	parts := make([]any, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + fileID})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"mapping": map[string]any{"tool-message": map[string]any{"message": map[string]any{
		"author": map[string]any{"role": "tool"}, "create_time": 1,
		"metadata": map[string]any{"async_task_type": "image_gen"}, "content": map[string]any{"parts": parts},
	}}}})
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
