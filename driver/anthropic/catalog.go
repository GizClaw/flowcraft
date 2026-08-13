package anthropic

import "maps"

// catalogEntry describes one model's compile-time capabilities.
type catalogEntry struct {
	vision           bool
	reasoning        bool
	deprecated       bool
	replacement      string
	reasoningLevels  bool
	reasoningDisable bool
}

// catalog is the built-in model list, aligned with the Claude lineup of
// July 2026.
var catalog = map[string]catalogEntry{
	"claude-fable-5":  {vision: true, reasoning: true, reasoningLevels: true, reasoningDisable: true},
	"claude-opus-5":   {vision: true, reasoning: true, reasoningLevels: true, reasoningDisable: true},
	"claude-sonnet-5": {vision: true, reasoning: true, reasoningLevels: true, reasoningDisable: true},
	"claude-haiku-4-5": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
	},
	"claude-haiku-4-5-20251001": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
	},

	"claude-opus-4-8": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
		deprecated: true, replacement: "claude-opus-5",
	},
	"claude-opus-4-7": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
		deprecated: true, replacement: "claude-opus-5",
	},
	"claude-sonnet-4-6": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
		deprecated: true, replacement: "claude-sonnet-5",
	},
	"claude-sonnet-4-5": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
		deprecated: true, replacement: "claude-sonnet-5",
	},
	"claude-opus-4-1": {
		vision: true, reasoning: true,
		reasoningLevels: true, reasoningDisable: true,
		deprecated: true, replacement: "claude-opus-5",
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, model := range spec.Models {
		models[model.Name] = catalogEntry{
			vision:           model.Vision,
			reasoning:        model.Reasoning,
			reasoningLevels:  model.Reasoning,
			reasoningDisable: model.Reasoning,
		}
	}
	return models, nil
}
