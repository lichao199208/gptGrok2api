package model

import "strings"

// IsOpenAIImage identifies the image models served by the ChatGPT account pool.
// Keep this list explicit so unknown models cannot fall through to a different
// provider or be treated as text conversation models.
func IsOpenAIImage(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "gpt-image-2", "gpt-image-2.5":
		return true
	default:
		return false
	}
}
