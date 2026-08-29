package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
)

type ConsoleEvent struct {
	Type  string
	Delta string
	Usage map[string]any
	Done  bool
	Err   *UpstreamError
}

var consoleModels = map[string]string{
	"grok-4.3-console":                     "grok-4.3",
	"grok-4.3-low":                         "grok-4.3",
	"grok-4.3-medium":                      "grok-4.3",
	"grok-4.3-high":                        "grok-4.3",
	"grok-4.20-0309-reasoning-console":     "grok-4.20-0309-reasoning",
	"grok-4.20-0309-console":               "grok-4.20-0309",
	"grok-4.20-0309-non-reasoning-console": "grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent-console":        "grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-low":            "grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-medium":         "grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-high":           "grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-xhigh":          "grok-4.20-multi-agent-0309",
	"grok-build-console":                   "grok-build-0.1",
}

var fixedEffort = map[string]string{
	"grok-4.3-low":                 "low",
	"grok-4.3-medium":              "medium",
	"grok-4.3-high":                "high",
	"grok-4.20-multi-agent-low":    "low",
	"grok-4.20-multi-agent-medium": "medium",
	"grok-4.20-multi-agent-high":   "high",
	"grok-4.20-multi-agent-xhigh":  "xhigh",
}

func BuildConsolePayload(messages []Message, model, reasoningEffort string, temperature, topP *float64, maxOutputTokens *int, stream bool) (map[string]any, error) {
	consoleModel, ok := consoleModels[model]
	if !ok {
		consoleModel = model
	}
	input := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "developer" {
			role = "system"
		}
		if role == "" {
			role = "user"
		}
		blocks := consoleContent(message.Content)
		if len(blocks) > 0 {
			input = append(input, map[string]any{"role": role, "content": blocks})
		}
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("input cannot be empty")
	}
	effort := fixedEffort[model]
	if effort == "" {
		effort = map[string]string{
			"none": "none", "minimal": "low", "low": "low",
			"medium": "medium", "high": "high", "xhigh": "xhigh",
		}[strings.ToLower(strings.TrimSpace(reasoningEffort))]
		if effort == "" {
			effort = "medium"
		}
	}
	maxTokens := 1000000
	if consoleModel == "grok-4.20-multi-agent-0309" {
		maxTokens = 2000000
	}
	if consoleModel == "grok-build-0.1" {
		maxTokens = 256000
	}
	if maxOutputTokens != nil {
		if *maxOutputTokens <= 0 {
			return nil, fmt.Errorf("max_output_tokens must be positive")
		}
		maxTokens = *maxOutputTokens
	}
	payload := map[string]any{
		"model":             consoleModel,
		"input":             input,
		"max_output_tokens": maxTokens,
		"temperature":       valueOrDefault(temperature, 0.7),
		"top_p":             valueOrDefault(topP, 0.95),
		"store":             false,
		"include":           []string{"reasoning.encrypted_content"},
		"stream":            stream,
	}
	if consoleModel == "grok-4.3" || consoleModel == "grok-4.20-multi-agent-0309" {
		payload["reasoning"] = map[string]any{"effort": effort}
	}
	if consoleSupportsSearch(consoleModel) {
		payload["tools"] = []map[string]any{
			{"type": "web_search", "enable_image_understanding": true},
			{"type": "x_search", "enable_video_understanding": true},
		}
		payload["tool_choice"] = "auto"
	}
	return payload, nil
}

func ParseConsoleLine(line string) (string, string) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "event:") {
		return "event", strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	}
	if strings.HasPrefix(line, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return "done", ""
		}
		return "data", data
	}
	return "skip", ""
}

func ResponsesInputMessages(input any, instructions string) []Message {
	messages := make([]Message, 0, 4)
	if strings.TrimSpace(instructions) != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch typed := input.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			messages = append(messages, Message{Role: "user", Content: typed})
		}
	case []any:
		for _, raw := range typed {
			if object, ok := raw.(map[string]any); ok {
				role := stringValue(object["role"])
				if role == "" {
					role = "user"
				}
				messages = append(messages, Message{Role: role, Content: object["content"]})
				continue
			}
			if text := stringValue(raw); text != "" {
				messages = append(messages, Message{Role: "user", Content: text})
			}
		}
	}
	return messages
}

func ScanConsole(scanner *bufio.Scanner, callback func(ConsoleEvent) error) error {
	currentEvent := ""
	for scanner.Scan() {
		kind, value := ParseConsoleLine(scanner.Text())
		switch kind {
		case "event":
			currentEvent = value
		case "done":
			if err := callback(ConsoleEvent{Type: "done", Done: true}); err != nil {
				return err
			}
			return nil
		case "data":
			event, err := decodeConsoleEvent(currentEvent, value)
			if err != nil {
				return err
			}
			if err := callback(event); err != nil {
				return err
			}
			currentEvent = ""
		}
	}
	return scanner.Err()
}

func decodeConsoleEvent(eventType, data string) (ConsoleEvent, error) {
	var object map[string]any
	if err := json.Unmarshal([]byte(data), &object); err != nil {
		return ConsoleEvent{}, nil
	}
	if eventType == "response.output_text.delta" {
		delta, _ := object["delta"].(string)
		return ConsoleEvent{Type: eventType, Delta: delta}, nil
	}
	if eventType == "response.completed" {
		response, _ := object["response"].(map[string]any)
		usage, _ := response["usage"].(map[string]any)
		return ConsoleEvent{Type: eventType, Usage: usage, Done: true}, nil
	}
	if eventType == "error" || eventType == "response.failed" || eventType == "response.incomplete" {
		message := stringValue(object["message"])
		if raw, ok := object["error"].(map[string]any); ok {
			message = firstString(raw, "message", "error")
		}
		if response, ok := object["response"].(map[string]any); ok {
			if raw, ok := response["error"].(map[string]any); ok {
				message = firstString(raw, "message", "error")
			}
		}
		return ConsoleEvent{Type: eventType, Err: &UpstreamError{Status: 502, Message: message, Body: data}}, nil
	}
	return ConsoleEvent{Type: eventType}, nil
}

func consoleContent(value any) []map[string]any {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []map[string]any{{"type": "input_text", "text": typed}}
	case []any:
		blocks := make([]map[string]any, 0, len(typed))
		for _, raw := range typed {
			object, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch object["type"] {
			case "text":
				blocks = append(blocks, map[string]any{"type": "input_text", "text": stringValue(object["text"])})
			case "image_url", "input_image":
				image := object["image_url"]
				url := stringValue(image)
				if nested, ok := image.(map[string]any); ok {
					url = stringValue(nested["url"])
				}
				if url != "" {
					blocks = append(blocks, map[string]any{"type": "input_image", "image_url": url})
				}
			}
		}
		return blocks
	default:
		return nil
	}
}

func consoleSupportsSearch(model string) bool {
	switch model {
	case "grok-4.20-multi-agent-0309", "grok-4.20-0309", "grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning", "grok-4.3", "grok-build-0.1":
		return true
	default:
		return false
	}
}

func valueOrDefault(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
