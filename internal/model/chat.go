package model

import "strings"

type ChatRoute struct {
	Mode           string
	PoolCandidates []string
	Console        bool
	OpenAI         bool
	Image          bool
}

func ResolveChat(id string) (ChatRoute, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ChatRoute{}, false
	}
	// Keep the legacy chat-completions compatibility path for image clients,
	// while the model remains hidden from the normal chat catalog.
	if id == "gpt-image-2" {
		return ChatRoute{OpenAI: true, Image: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	if isOpenAIChatModel(id) {
		return ChatRoute{OpenAI: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	if strings.HasSuffix(id, "-console") || strings.Contains(id, "-console-") {
		return ChatRoute{Mode: "console", PoolCandidates: []string{"basic"}, Console: true}, true
	}
	switch id {
	case "grok-4.20-0309-non-reasoning", "grok-4.20-fast", "grok-4.3-fast":
		return ChatRoute{Mode: "fast", PoolCandidates: []string{"basic", "super", "heavy"}}, true
	case "grok-4.20-0309-non-reasoning-super":
		return ChatRoute{Mode: "fast", PoolCandidates: []string{"super", "heavy"}}, true
	case "grok-4.20-0309-non-reasoning-heavy":
		return ChatRoute{Mode: "fast", PoolCandidates: []string{"heavy"}}, true
	case "grok-4.20-0309", "grok-4.20-auto":
		return ChatRoute{Mode: "auto", PoolCandidates: []string{"super", "heavy"}}, true
	case "grok-4.20-0309-super":
		return ChatRoute{Mode: "auto", PoolCandidates: []string{"super", "heavy"}}, true
	case "grok-4.20-0309-heavy", "grok-4.20-heavy":
		return ChatRoute{Mode: "heavy", PoolCandidates: []string{"heavy"}}, true
	case "grok-4.20-0309-reasoning", "grok-4.20-expert":
		return ChatRoute{Mode: "expert", PoolCandidates: []string{"super", "heavy"}}, true
	case "grok-4.20-0309-reasoning-super":
		return ChatRoute{Mode: "expert", PoolCandidates: []string{"super", "heavy"}}, true
	case "grok-4.20-0309-reasoning-heavy":
		return ChatRoute{Mode: "expert", PoolCandidates: []string{"heavy"}}, true
	case "grok-4.20-multi-agent-0309":
		return ChatRoute{Mode: "heavy", PoolCandidates: []string{"heavy"}}, true
	case "grok-4.3-beta":
		return ChatRoute{Mode: "grok-420-computer-use-sa", PoolCandidates: []string{"super", "heavy"}}, true
	}
	return ChatRoute{}, false
}

func isOpenAIChatModel(id string) bool {
	switch strings.TrimSpace(id) {
	case "auto",
		"gpt-5",
		"gpt-5-1",
		"gpt-5-2",
		"gpt-5-3",
		"gpt-5-3-mini",
		"gpt-5-5",
		"gpt-5-6",
		"gpt-5-6-sol",
		"gpt-5-6-terra",
		"gpt-5-6-luna",
		"gpt-5-mini":
		return true
	default:
		return false
	}
}
