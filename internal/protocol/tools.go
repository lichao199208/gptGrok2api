package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

var (
	toolRootPattern   = regexp.MustCompile(`(?is)<tool_calls\s*>(.*?)</tool_calls\s*>`)
	toolCallPattern   = regexp.MustCompile(`(?is)<tool_call\s*>(.*?)</tool_call\s*>`)
	toolNamePattern   = regexp.MustCompile(`(?is)<tool_name\s*>(.*?)</tool_name\s*>`)
	toolParamsPattern = regexp.MustCompile(`(?is)<parameters\s*>(.*?)</parameters\s*>`)
)

func BuildToolSystemPrompt(tools []map[string]any, choice any) string {
	parts := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name := toolStringValue(function["name"])
		if name == "" {
			name = toolStringValue(tool["name"])
		}
		if name == "" {
			continue
		}
		description := toolStringValue(function["description"])
		params := function["parameters"]
		if params == nil {
			params = tool["parameters"]
		}
		rawParams, _ := json.Marshal(params)
		item := "Tool: " + name
		if description != "" {
			item += "\nDescription: " + description
		}
		if params != nil {
			item += "\nParameters: " + string(rawParams)
		}
		parts = append(parts, item)
	}
	choiceText := "WHEN TO CALL: Call a tool when it is clearly needed. Otherwise respond in plain text."
	switch typed := choice.(type) {
	case string:
		switch typed {
		case "none":
			choiceText = "WHEN TO CALL: Do NOT call any tools. Respond in plain text only."
		case "required":
			choiceText = "WHEN TO CALL: You MUST output a <tool_calls> XML block."
		}
	case map[string]any:
		if function, ok := typed["function"].(map[string]any); ok && toolStringValue(function["name"]) != "" {
			choiceText = "WHEN TO CALL: You MUST call the tool named \"" + toolStringValue(function["name"]) + "\"."
		}
	}
	return "You have access to the following tools.\n\nAVAILABLE TOOLS:\n" + strings.Join(parts, "\n\n") + `

TOOL CALL FORMAT: output only this XML when calling a tool. Do not use markdown fences.
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters>{"key":"value"}</parameters></tool_call></tool_calls>
` + choiceText
}

func ToolNames(tools []map[string]any) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name := toolStringValue(function["name"])
		if name == "" {
			name = toolStringValue(tool["name"])
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func InjectToolPrompt(message, prompt string) string { return "[system]: " + prompt + "\n\n" + message }

func ParseToolCalls(text string, allowed []string) []ToolCall {
	if !strings.Contains(strings.ToLower(text), "tool_call") && !strings.Contains(strings.ToLower(text), "tool_calls") {
		return nil
	}
	allowedSet := map[string]bool{}
	for _, value := range allowed {
		allowedSet[value] = true
	}
	root := toolRootPattern.FindStringSubmatch(text)
	if len(root) == 0 {
		return parseJSONToolCalls(text, allowedSet)
	}
	calls := make([]ToolCall, 0)
	for _, block := range toolCallPattern.FindAllStringSubmatch(root[1], -1) {
		nameMatch := toolNamePattern.FindStringSubmatch(block[1])
		if len(nameMatch) != 2 {
			continue
		}
		name := strings.TrimSpace(nameMatch[1])
		if name == "" || (len(allowedSet) > 0 && !allowedSet[name]) {
			continue
		}
		args := "{}"
		if params := toolParamsPattern.FindStringSubmatch(block[1]); len(params) == 2 {
			candidate := strings.TrimSpace(params[1])
			var object any
			if json.Unmarshal([]byte(candidate), &object) == nil {
				encoded, _ := json.Marshal(object)
				args = string(encoded)
			}
		}
		calls = append(calls, ToolCall{ID: fmt.Sprintf("call_%d", time.Now().UnixNano()+int64(len(calls))), Name: name, Arguments: args})
	}
	return calls
}

func parseJSONToolCalls(text string, allowed map[string]bool) []ToolCall {
	start := strings.Index(text, "{")
	if start < 0 {
		return nil
	}
	var object map[string]any
	if json.Unmarshal([]byte(text[start:]), &object) != nil {
		return nil
	}
	raw, _ := object["tool_calls"].([]any)
	result := make([]ToolCall, 0, len(raw))
	for _, item := range raw {
		value, _ := item.(map[string]any)
		name := toolStringValue(value["name"])
		if name == "" {
			name = toolStringValue(value["tool_name"])
		}
		if name == "" || (len(allowed) > 0 && !allowed[name]) {
			continue
		}
		args := value["arguments"]
		if args == nil {
			args = value["input"]
		}
		encoded, _ := json.Marshal(args)
		result = append(result, ToolCall{ID: fmt.Sprintf("call_%d", time.Now().UnixNano()+int64(len(result))), Name: name, Arguments: string(encoded)})
	}
	return result
}

func toolStringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
