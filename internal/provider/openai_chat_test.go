package provider

import (
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestOpenAIChatPayloadUsesRawMessageText(t *testing.T) {
	payload := openAIChatPayload(protocol.ChatRequest{
		Model: "gpt-5-6",
		Messages: []protocol.Message{
			{Role: "user", Content: "请回复Go测试成功"},
		},
	})
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("unexpected messages: %#v", payload["messages"])
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0] != "请回复Go测试成功" {
		t.Fatalf("message text was decorated: %#v", parts)
	}
	if strings.Contains(parts[0].(string), "[user]") {
		t.Fatalf("message contains protocol decoration: %q", parts[0])
	}
}

func TestOpenAIChatStateReturnsSSEDetailAsError(t *testing.T) {
	state := &openAIChatState{}
	_, err := state.event(map[string]any{"detail": "Invalid conversation body"})
	if err == nil || !strings.Contains(err.Error(), "Invalid conversation body") {
		t.Fatalf("expected upstream detail error, got %v", err)
	}
}
