package anthropic

import "maps"

// catalogEntry describes one model's compile-time capabilities.
type catalogEntry struct {
	vision      bool
	reasoning   bool
	deprecated  bool
	replacement string
}

// catalog is the built-in model list, aligned with the Claude lineup of
// July 2026 (Fable 5 frontier, Opus 5 flagship, Sonnet 5 default, Haiku 4.5
// fast tier). Every listed model accepts image input and the reasoning
// effort knob. Deployments extend or override it via Spec.Models — a custom
// declaration replaces the same-named built-in entirely.
var catalog = map[string]catalogEntry{
	"claude-fable-5":  {vision: true, reasoning: true},
	"claude-opus-5":   {vision: true, reasoning: true},
	"claude-sonnet-5": {vision: true, reasoning: true},
	"claude-haiku-4-5": {
		vision:    true,
		reasoning: true,
	},
	"claude-haiku-4-5-20251001": {
		vision:    true,
		reasoning: true,
	},

	// Retired tiers, kept routable with explicit replacement pointers.
	"claude-opus-4-8": {
		vision: true, reasoning: true,
		deprecated: true, replacement: "claude-opus-5",
	},
	"claude-opus-4-7": {
		vision: true, reasoning: true,
		deprecated: true, replacement: "claude-opus-5",
	},
	"claude-sonnet-4-6": {
		vision: true, reasoning: true,
		deprecated: true, replacement: "claude-sonnet-5",
	},
	"claude-sonnet-4-5": {
		vision: true, reasoning: true,
		deprecated: true, replacement: "claude-sonnet-5",
	},
	"claude-opus-4-1": {
		vision: true, reasoning: true,
		deprecated: true, replacement: "claude-opus-5",
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog. A custom
// entry replaces the same-named built-in entirely.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, model := range spec.Models {
		models[model.Name] = catalogEntry{
			vision:    model.Vision,
			reasoning: model.Reasoning,
		}
	}
	return models, nil
}
