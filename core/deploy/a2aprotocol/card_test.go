package a2aprotocol

import (
	"strings"
	"testing"
)

// A card is a promise to clients: the fields that make it one are required at
// load time, not filled in later.
func TestDecodeSettingsRequiresThePromise(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"no card":       {},
		"no version":    {"card": map[string]any{"url": "https://x"}},
		"no url":        {"card": map[string]any{"version": "1.0.0"}},
		"unknown field": {"card": map[string]any{"version": "1.0.0", "url": "https://x", "colour": "red"}},
	} {
		if _, err := DecodeSettings(settingsJSON(settings)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDecodeSettingsCarriesTheClaims(t *testing.T) {
	settings, err := DecodeSettings(settingsJSON(map[string]any{
		"card": map[string]any{
			"description": "story",
			"version":     "1.0.0",
			"url":         "https://host/agents/story",
			"skills":      []any{map[string]any{"id": "publish", "name": "发布"}},
		},
	}))
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	if settings.Card.Version != "1.0.0" || settings.Card.URL == "" {
		t.Errorf("card = %+v", settings.Card)
	}
	if len(settings.Card.Skills) != 1 || settings.Card.Skills[0].ID != "publish" {
		t.Errorf("skills = %+v", settings.Card.Skills)
	}
	// The name is the agent's, so the builder fills it; decode leaves it empty.
	if settings.Card.Name != "" {
		t.Errorf("name = %q, want it left to the builder", settings.Card.Name)
	}
}

func TestDecodeSettingsRejectsDuplicateSkills(t *testing.T) {
	_, err := DecodeSettings(settingsJSON(map[string]any{
		"card": map[string]any{
			"version": "1.0.0", "url": "https://x",
			"skills": []any{
				map[string]any{"id": "a", "name": "A"},
				map[string]any{"id": "a", "name": "B"},
			},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a duplicate complaint", err)
	}
}

// Streaming is a fact about the loop; modes and skills are promises and stay
// empty until a deployment makes them.
func TestCardClaimsOnlyWhatItCan(t *testing.T) {
	card, err := newCard(cardConfig{Name: "storyteller", Version: "1.0.0", URL: "https://example.test"})
	if err != nil {
		t.Fatalf("newCard: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Error("streaming = false, want true")
	}
	if len(card.DefaultInputModes) != 0 || len(card.Skills) != 0 {
		t.Errorf("modes/skills = %v/%v, want none: nothing was declared",
			card.DefaultInputModes, card.Skills)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Errorf("interfaces = %d, want the declared endpoint", len(card.SupportedInterfaces))
	}
}

func TestCardRequiresTheDeclaredFacts(t *testing.T) {
	if _, err := newCard(cardConfig{Version: "1", URL: "https://x"}); err == nil {
		t.Error("a card without a name was accepted")
	}
	if _, err := newCard(cardConfig{Name: "x", URL: "https://x"}); err == nil {
		t.Error("a card without a version was accepted")
	}
	if _, err := newCard(cardConfig{Name: "x", Version: "1"}); err == nil {
		t.Error("a card without a url was accepted")
	}
}
