package protocol

import (
	"strings"
	"testing"
)

func TestBuildGrokPayloadAndExtractMessage(t *testing.T) {
	temperature := 0.2
	topP := 0.8
	maxTokens := 64
	message := ExtractMessage([]Message{
		{Role: "system", Content: "Be concise"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "Hello"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "ignored"}},
		}},
	})
	if !strings.Contains(message, "[system]: Be concise") || !strings.Contains(message, "[user]: Hello") {
		t.Fatalf("unexpected message: %q", message)
	}
	payload := BuildGrokPayload(message, "fast", &temperature, &topP, &maxTokens)
	if payload["modeId"] != "fast" {
		t.Fatalf("unexpected mode: %#v", payload["modeId"])
	}
	metadata := payload["responseMetadata"].(map[string]any)
	override := metadata["modelConfigOverride"].(map[string]any)
	if override["maxTokens"] != maxTokens {
		t.Fatalf("unexpected override: %#v", override)
	}
}

func TestParseUpstreamChatFrames(t *testing.T) {
	events, err := ParseUpstreamLine(`data: {"result":{"response":{"token":"hello","messageTag":"final"}}}`)
	if err != nil || len(events) != 1 || events[0].Text != "hello" {
		t.Fatalf("unexpected text event: %#v %v", events, err)
	}
	events, err = ParseUpstreamLine(`{"result":{"response":{"token":"thinking","isThinking":true}}}`)
	if err != nil || len(events) != 1 || events[0].Thinking != "thinking" {
		t.Fatalf("unexpected thinking event: %#v %v", events, err)
	}
	_, err = ParseUpstreamLine(`data: {"error":{"code":8,"message":"rate limit"}}`)
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if upstream, ok := err.(*UpstreamError); !ok || upstream.Status != 429 {
		t.Fatalf("unexpected error: %#v", err)
	}
}
