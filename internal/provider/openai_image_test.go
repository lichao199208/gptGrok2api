package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

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
