package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Message struct {
	Role      string `json:"role"`
	Content   any    `json:"content"`
	ToolCalls []any  `json:"tool_calls,omitempty"`
}

type ChatRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Stream          bool             `json:"stream"`
	Size            string           `json:"size,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort"`
	Temperature     *float64         `json:"temperature"`
	TopP            *float64         `json:"top_p"`
	MaxTokens       *int             `json:"max_tokens"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
}

type ResponsesRequest struct {
	Model           string           `json:"model"`
	Input           any              `json:"input"`
	Instructions    string           `json:"instructions"`
	Stream          bool             `json:"stream"`
	Reasoning       map[string]any   `json:"reasoning"`
	Temperature     *float64         `json:"temperature"`
	TopP            *float64         `json:"top_p"`
	MaxOutputTokens *int             `json:"max_output_tokens"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
}

type UpstreamEvent struct {
	Text          string
	Thinking      string
	SoftStop      bool
	UpstreamError *UpstreamError
}

type UpstreamError struct {
	Status        int
	Message       string
	Body          string
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *UpstreamError) Error() string {
	return e.Message
}

func ExtractMessage(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		text := contentText(message.Content)
		if text == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s]: %s", role, text))
	}
	return strings.Join(parts, "\n\n")
}

func BuildGrokPayload(message, mode string, temperature *float64, topP *float64, maxTokens *int) map[string]any {
	payload := map[string]any{
		"collectionIds": []any{},
		"connectors":    []any{},
		"deviceEnvInfo": map[string]any{
			"darkModeEnabled":  false,
			"devicePixelRatio": 2,
			"screenHeight":     1329,
			"screenWidth":      2056,
			"viewportHeight":   1083,
			"viewportWidth":    2056,
		},
		"disableMemory":               true,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []any{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"message":                     message,
		"modeId":                      mode,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		"temporary":                   true,
		"toolOverrides": map[string]any{
			"gmailSearch":           false,
			"googleCalendarSearch":  false,
			"outlookSearch":         false,
			"outlookCalendarSearch": false,
			"googleDriveSearch":     false,
		},
	}
	override := map[string]any{}
	if temperature != nil {
		override["temperature"] = *temperature
	}
	if topP != nil {
		override["topP"] = *topP
	}
	if maxTokens != nil {
		override["maxTokens"] = *maxTokens
	}
	if len(override) > 0 {
		payload["responseMetadata"].(map[string]any)["modelConfigOverride"] = override
	}
	return payload
}

func ParseUpstreamLine(line string) ([]UpstreamEvent, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") {
		return nil, nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		return []UpstreamEvent{{SoftStop: true}}, nil
	}
	if !strings.HasPrefix(line, "{") {
		return nil, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		return nil, nil
	}
	if rawError, ok := envelope["error"].(map[string]any); ok {
		message := firstString(rawError, "message", "error")
		status := 502
		code := fmt.Sprint(rawError["code"])
		lower := strings.ToLower(message)
		if code == "8" || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") {
			status = 429
		}
		return nil, &UpstreamError{Status: status, Message: message, Body: line}
	}
	result, _ := envelope["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	event := UpstreamEvent{}
	if token, ok := response["token"].(string); ok {
		if response["isThinking"] == true {
			event.Thinking = token
		} else if response["messageTag"] == "final" || response["messageTag"] == nil || response["messageTag"] == "" {
			event.Text = token
		}
	}
	if response["isSoftStop"] == true || response["finalMetadata"] != nil {
		event.SoftStop = true
	}
	return []UpstreamEvent{event}, nil
}

func ScanUpstream(body *bufio.Scanner, onEvent func(UpstreamEvent) error) error {
	for body.Scan() {
		events, err := ParseUpstreamLine(body.Text())
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return body.Err()
}

func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			if object, ok := raw.(map[string]any); ok {
				if object["type"] == "text" {
					if text, ok := object["text"].(string); ok {
						parts = append(parts, strings.TrimSpace(text))
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				return text
			}
		}
	}
	return "upstream stream error"
}
