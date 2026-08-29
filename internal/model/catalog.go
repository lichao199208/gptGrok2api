package model

import "time"

type Capability uint8

const (
	Chat Capability = 1 << iota
	Image
	ImageEdit
	Video
	ConsoleChat
)

type Spec struct {
	ID         string
	Name       string
	OwnedBy    string
	Created    int64
	Capability Capability
	Enabled    bool
}

func (s Spec) Public() map[string]any {
	return map[string]any{
		"id":       s.ID,
		"object":   "model",
		"created":  s.Created,
		"owned_by": s.OwnedBy,
		"name":     s.Name,
	}
}

func Catalog() []Spec {
	created := time.Now().Unix()
	items := []Spec{
		{"auto", "Auto", "openai", created, Chat, true},
		{"gpt-5", "GPT-5", "openai", created, Chat, true},
		{"gpt-5-1", "GPT-5.1", "openai", created, Chat, true},
		{"gpt-5-2", "GPT-5.2", "openai", created, Chat, true},
		{"gpt-5-3", "GPT-5.3", "openai", created, Chat, true},
		{"gpt-5-3-mini", "GPT-5.3 Mini", "openai", created, Chat, true},
		{"gpt-5-5", "GPT-5.5", "openai", created, Chat, true},
		{"gpt-5-6", "GPT-5.6", "openai", created, Chat, true},
		{"gpt-5-6-sol", "GPT-5.6 Sol", "openai", created, Chat, true},
		{"gpt-5-6-terra", "GPT-5.6 Terra", "openai", created, Chat, true},
		{"gpt-5-6-luna", "GPT-5.6 Luna", "openai", created, Chat, true},
		{"gpt-5-mini", "GPT-5 Mini", "openai", created, Chat, true},
		{"grok-4.20-0309-non-reasoning", "Grok 4.20 0309 Non-Reasoning", "xai", created, Chat, true},
		{"grok-4.20-0309", "Grok 4.20 0309", "xai", created, Chat, true},
		{"grok-4.20-0309-reasoning", "Grok 4.20 0309 Reasoning", "xai", created, Chat, true},
		{"grok-4.20-fast", "Grok 4.20 Fast", "xai", created, Chat, true},
		{"grok-4.3-fast", "Grok 4.3 Fast", "xai", created, Chat, true},
		{"grok-4.20-auto", "Grok 4.20 Auto", "xai", created, Chat, true},
		{"grok-4.20-expert", "Grok 4.20 Expert", "xai", created, Chat, true},
		{"grok-4.20-heavy", "Grok 4.20 Heavy", "xai", created, Chat, true},
		{"grok-4.3-beta", "Grok 4.3 Beta", "xai", created, Chat, true},
		{"grok-imagine-image-lite", "Grok Imagine Image Lite", "xai", created, Image, true},
		{"gpt-image-2", "GPT Image 2", "openai-compatible", created, Image, true},
		{"grok-imagine-image", "Grok Imagine Image", "xai", created, Image, true},
		{"grok-imagine-image-pro", "Grok Imagine Image Pro", "xai", created, Image, true},
		{"grok-imagine-image-edit", "Grok Imagine Image Edit", "xai", created, ImageEdit, true},
		{"grok-imagine-video", "Grok Imagine Video", "xai", created, Video, true},
		{"grok-4.3-console", "Grok 4.3 (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.3-low", "Grok 4.3 Low Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.3-medium", "Grok 4.3 Medium Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.3-high", "Grok 4.3 High Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-reasoning-console", "Grok 4.20 0309 Reasoning (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-console", "Grok 4.20 0309 (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-console", "Grok 4.20 Multi-Agent (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-low", "Grok 4.20 Multi-Agent Low", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-medium", "Grok 4.20 Multi-Agent Medium", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-high", "Grok 4.20 Multi-Agent High", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-xhigh", "Grok 4.20 Multi-Agent XHigh", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-non-reasoning-console", "Grok 4.20 0309 Non-Reasoning (Console)", "xai", created, ConsoleChat, true},
		{"grok-build-console", "Grok Build 0.1 (Console)", "xai", created, ConsoleChat, true},
	}
	return items
}

func Find(items []Spec, id string) (Spec, bool) {
	for _, item := range items {
		if item.Enabled && item.ID == id {
			return item, true
		}
	}
	return Spec{}, false
}
