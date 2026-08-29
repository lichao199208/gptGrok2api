package model

import "testing"

func TestCatalogContainsCoreModels(t *testing.T) {
	items := Catalog()
	for _, id := range []string{"grok-4.20-fast", "grok-imagine-image", "grok-imagine-video"} {
		if _, ok := Find(items, id); !ok {
			t.Fatalf("catalog missing %q", id)
		}
	}
}

func TestImageModelChatCompatibilityRoute(t *testing.T) {
	route, ok := ResolveChat("gpt-image-2")
	if !ok || !route.OpenAI || !route.Image {
		t.Fatal("gpt-image-2 must retain its image chat-completions compatibility route")
	}
	for _, item := range Catalog() {
		if item.ID == "gpt-image-2" && item.Capability&Chat != 0 {
			t.Fatal("gpt-image-2 must remain hidden from the normal chat catalog")
		}
	}
}
