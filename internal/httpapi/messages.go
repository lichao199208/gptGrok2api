package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		System      any              `json:"system"`
		MaxTokens   *int             `json:"max_tokens"`
		Stream      bool             `json:"stream"`
		Temperature *float64         `json:"temperature"`
		TopP        *float64         `json:"top_p"`
		Tools       []map[string]any `json:"tools"`
		ToolChoice  any              `json:"tool_choice"`
		Thinking    map[string]any   `json:"thinking"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Model == "" || len(request.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "model and messages are required", "invalid_request_error")
		return
	}
	spec, ok := model.Find(s.catalog, request.Model)
	if !ok || spec.Capability&model.Chat == 0 && spec.Capability&model.ConsoleChat == 0 {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	messages := make([]protocol.Message, 0, len(request.Messages)+1)
	if system := anthropicText(request.System); system != "" {
		messages = append(messages, protocol.Message{Role: "system", Content: system})
	}
	for _, raw := range request.Messages {
		role := stringValue(raw["role"])
		content := raw["content"]
		messages = append(messages, protocol.Message{Role: role, Content: content})
	}
	chat := protocol.ChatRequest{Model: request.Model, Messages: messages, Stream: false, Temperature: request.Temperature, TopP: request.TopP, MaxTokens: request.MaxTokens}
	chat.Tools = convertAnthropicTools(request.Tools)
	chat.ToolChoice = convertAnthropicChoice(request.ToolChoice)
	if request.Thinking != nil && strings.EqualFold(stringValue(request.Thinking["type"]), "disabled") {
		chat.ReasoningEffort = "none"
	}
	if request.Stream {
		chat.Stream = true
	}
	if spec.Capability&model.ConsoleChat != 0 {
		s.consoleChatCompletions(w, r, chat)
		return
	}
	if request.Stream {
		recorder := &responseCapture{header: make(http.Header)}
		s.completeChat(recorder, r, chat, model.ChatRoute{Mode: "fast", PoolCandidates: []string{"basic", "super", "heavy"}})
		s.writeAnthropicStream(w, recorder, request.Model)
		return
	}
	recorder := &responseCapture{header: make(http.Header)}
	s.completeChat(recorder, r, chat, model.ChatRoute{Mode: "fast", PoolCandidates: []string{"basic", "super", "heavy"}})
	s.writeAnthropicResponse(w, recorder, request.Model)
}

func (s *Server) writeAnthropicResponse(w http.ResponseWriter, recorder *responseCapture, modelName string) {
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if json.Unmarshal(recorder.body.Bytes(), &chat) != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	choices, _ := chat["choices"].([]any)
	content := make([]map[string]any, 0, 1)
	stop := "end_turn"
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if calls, ok := message["tool_calls"].([]any); ok {
			stop = "tool_use"
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				var input any
				_ = json.Unmarshal([]byte(stringValue(fn["arguments"])), &input)
				content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": input})
			}
		} else {
			content = append(content, map[string]any{"type": "text", "text": stringValue(message["content"])})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": "msg_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-"), "type": "message", "role": "assistant", "model": modelName, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": chatUsage(chat, "prompt_tokens"), "output_tokens": chatUsage(chat, "completion_tokens")}})
}

func (s *Server) writeAnthropicStream(w http.ResponseWriter, recorder *responseCapture, modelName string) {
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	_ = json.Unmarshal(recorder.body.Bytes(), &chat)
	id := "msg_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-")
	writeEventSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": modelName, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": chatUsage(chat, "prompt_tokens"), "output_tokens": 0}}})
	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if calls, ok := message["tool_calls"].([]any); ok {
			for index, raw := range calls {
				call, _ := raw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				writeEventSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": map[string]any{}}})
				writeEventSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": fn["arguments"]}})
				writeEventSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			}
		} else {
			text := stringValue(message["content"])
			writeEventSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			writeEventSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
			writeEventSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		}
	}
	writeEventSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": chatUsage(chat, "completion_tokens")}})
	writeEventSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func anthropicText(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if list, ok := value.([]any); ok {
		parts := []string{}
		for _, raw := range list {
			object, _ := raw.(map[string]any)
			if stringValue(object["type"]) == "text" {
				parts = append(parts, stringValue(object["text"]))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func convertAnthropicTools(tools []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{"type": "function", "function": map[string]any{"name": toolString(tool, "name"), "description": toolString(tool, "description"), "parameters": tool["input_schema"]}})
	}
	return result
}
func convertAnthropicChoice(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if stringValue(object["type"]) == "tool" {
		return map[string]any{"type": "function", "function": map[string]any{"name": object["name"]}}
	}
	return value
}
func toolString(object map[string]any, key string) string { return stringValue(object[key]) }
func chatUsage(chat map[string]any, key string) int {
	usage, _ := chat["usage"].(map[string]any)
	value, _ := usage[key].(float64)
	return int(value)
}

var _ = time.Now
