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
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOpenAIImageUploadRetriesProxyThenFallsBackToDirect(t *testing.T) {
	payload := []byte("replayable-image-payload")
	proxyAttempts := 0
	directAttempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, payload) {
			t.Fatalf("upload body changed between retries: %q", raw)
		}
		if proxyruntime.URLFromContext(request.Context()) != "" {
			proxyAttempts++
			return nil, errors.New("read: connection reset by peer")
		}
		directAttempts++
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	imageClient := NewOpenAIImage("https://chatgpt.invalid", client, nil, 10*time.Second)
	ctx := proxyruntime.WithURL(context.Background(), "http://proxy.invalid:8080")
	err := imageClient.uploadInputBlob(ctx, accounts.Account{}, "https://storage.invalid/upload", payload, map[string]string{"Content-Type": "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if proxyAttempts != 2 || directAttempts != 1 {
		t.Fatalf("unexpected attempts: proxy=%d direct=%d", proxyAttempts, directAttempts)
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
