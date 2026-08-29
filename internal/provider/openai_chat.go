package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// OpenAIChat implements the authenticated ChatGPT Web conversation endpoint
// for the OpenAI JWT account pool. It intentionally shares the Sentinel and
// browser transport with OpenAIImage.
type OpenAIChat struct {
	Image *OpenAIImage
}

func NewOpenAIChat(image *OpenAIImage) *OpenAIChat {
	return &OpenAIChat{Image: image}
}

type OpenAIChatEvent struct {
	Text     string
	Thinking string
	Done     bool
}

func (c *OpenAIChat) Complete(ctx context.Context, account accounts.Account, request protocol.ChatRequest) (string, string, error) {
	var text strings.Builder
	var thinking strings.Builder
	err := c.Stream(ctx, account, request, func(event OpenAIChatEvent) error {
		text.WriteString(event.Text)
		thinking.WriteString(event.Thinking)
		return nil
	})
	return text.String(), thinking.String(), err
}

func (c *OpenAIChat) Stream(ctx context.Context, account accounts.Account, request protocol.ChatRequest, onEvent func(OpenAIChatEvent) error) error {
	if c == nil || c.Image == nil {
		return fmt.Errorf("OpenAI chat provider is not configured")
	}
	scripts, build, err := c.Image.bootstrap(ctx, account)
	if err != nil {
		return err
	}
	requirements, err := c.Image.chatRequirements(ctx, account, scripts, build)
	if err != nil {
		return err
	}
	payload := openAIChatPayload(request)
	headers := c.Image.requirementHeaders(requirements)
	headers["Accept"] = "text/event-stream"
	response, err := c.Image.do(ctx, "POST", "/backend-api/conversation", account, payload, headers, true)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	state := openAIChatState{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			return onEvent(OpenAIChatEvent{Done: true})
		}
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			continue
		}
		event, eventErr := state.event(value)
		if eventErr != nil {
			return eventErr
		}
		if event.Text != "" || event.Thinking != "" || event.Done {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return onEvent(OpenAIChatEvent{Done: true})
}

type openAIChatState struct {
	text string
}

func (s *openAIChatState) event(value any) (OpenAIChatEvent, error) {
	event := OpenAIChatEvent{}
	if object, ok := value.(map[string]any); ok {
		if detail, ok := object["detail"].(string); ok {
			return OpenAIChatEvent{}, &protocol.UpstreamError{
				Status:  400,
				Message: detail,
				Body:    detail,
			}
		}
		if rawError, ok := object["error"].(map[string]any); ok {
			status := 502
			if code := strings.ToLower(firstStringValue(rawError, "code", "type")); strings.Contains(code, "rate") {
				status = 429
			}
			return OpenAIChatEvent{}, &protocol.UpstreamError{
				Status:  status,
				Message: firstStringValue(rawError, "message", "error"),
				Body:    stringValue(rawError["message"]),
			}
		}
		if candidate := openAIAssistantText(object); candidate != "" {
			event.Text = appendDelta(&s.text, candidate)
		} else if delta, ok := object["v"].(string); ok && object["o"] == "append" {
			event.Text = delta
			s.text += delta
		}
		if object["is_complete"] == true || object["finished_successfully"] == true {
			event.Done = true
		}
	}
	return event, nil
}

func openAIChatPayload(request protocol.ChatRequest) map[string]any {
	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		text := openAIMessageText(message.Content)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{
			"id":          firstUUID(),
			"author":      map[string]any{"role": role},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     map[string]any{"content_type": "text", "parts": []any{text}},
		})
	}
	modelID := strings.TrimSpace(request.Model)
	if modelID == "auto" || modelID == "" {
		modelID = "auto"
	}
	payload := map[string]any{
		"action":                        "next",
		"messages":                      messages,
		"model":                         modelID,
		"parent_message_id":             firstUUID(),
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true,
		"history_and_training_disabled": true,
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"timezone":                      "Asia/Shanghai",
		"timezone_offset_min":           -480,
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          firstUUID(),
		"client_contextual_info":        map[string]any{"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560},
	}
	if request.ReasoningEffort != "" && request.ReasoningEffort != "none" {
		payload["thinking_effort"] = request.ReasoningEffort
	}
	return payload
}

func openAIMessageText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(part["type"]) == "text" {
				parts = append(parts, stringValue(part["text"]))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func openAIAssistantText(value map[string]any) string {
	for _, candidate := range []any{value, value["v"]} {
		object, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		message, ok := object["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		if role := strings.ToLower(stringValue(author["role"])); role != "" && role != "assistant" {
			continue
		}
		content, _ := message["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		var text strings.Builder
		for _, part := range parts {
			if value, ok := part.(string); ok {
				text.WriteString(value)
			}
		}
		if text.Len() == 0 {
			if value, ok := content["text"].(string); ok {
				text.WriteString(value)
			}
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

func appendDelta(current *string, candidate string) string {
	if candidate == "" {
		return ""
	}
	if strings.HasPrefix(candidate, *current) {
		delta := strings.TrimPrefix(candidate, *current)
		*current = candidate
		return delta
	}
	if strings.HasPrefix(*current, candidate) {
		return ""
	}
	*current += candidate
	return candidate
}
