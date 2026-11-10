package server

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
