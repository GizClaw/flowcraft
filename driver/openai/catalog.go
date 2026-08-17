package openai

import "maps"

// modelKind classifies catalog models by the operation family they serve.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
)

// apiMode selects the generate wire surface for one provider instance.
type apiMode string

const (
	apiResponses apiMode = "responses"
	apiChat      apiMode = "chat"
)

// catalogEntry is one model in the built-in catalog. Capability flags mirror
// ModelSpec so deployment-declared models behave identically.
type catalogEntry struct {
	kind      modelKind
	api       apiMode
	vision    bool
	reasoning bool
	// webSearch (generate) accepts the hosted web_search tool. Model
	// support is per model per the OpenAI model documentation; see
	// https://developers.openai.com/api/docs/models.
	webSearch   bool
	dimensions  bool
	deprecated  bool
	replacement string
	// maxInputTokens caps the input context (prompt plus prior turns) in
	// tokens; zero means undeclared. Generate values mirror the context
	// window on https://developers.openai.com/api/docs/models; embedding
	// values mirror the per-request input limit.
	maxInputTokens int
}

// catalog is the built-in model list, aligned with the OpenAI model lineup
// of July 2026 (GPT-5.6 family flagship). Deployments extend or override it
// via Spec.Models.
var catalog = map[string]catalogEntry{
	// Generate — GPT-5.6 flagship family (reasoning + vision).
	"gpt-5.6-sol":   {kind: kindGenerate, vision: true, reasoning: true, webSearch: true, maxInputTokens: 1_050_000},
	"gpt-5.6-terra": {kind: kindGenerate, vision: true, reasoning: true, webSearch: true, maxInputTokens: 1_050_000},
	"gpt-5.6-luna":  {kind: kindGenerate, vision: true, reasoning: true, webSearch: true, maxInputTokens: 1_050_000},
	// Generate — previous generations, superseded but available.
	"gpt-5.5": {
		kind: kindGenerate, vision: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "gpt-5.6-sol",
		maxInputTokens: 1_050_000,
	},
	"gpt-5.4": {
		kind: kindGenerate, vision: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "gpt-5.6-sol",
		maxInputTokens: 1_050_000,
	},
	"gpt-5.4-mini": {
		kind: kindGenerate, vision: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "gpt-5.6-terra",
		maxInputTokens: 400_000,
	},
	"gpt-5.4-nano": {
		kind: kindGenerate, vision: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "gpt-5.6-luna",
		maxInputTokens: 400_000,
	},
	// Generate — GPT-4.1 line: vision without the reasoning control.
	"gpt-4.1":      {kind: kindGenerate, vision: true, webSearch: true, maxInputTokens: 1_047_576},
	"gpt-4.1-mini": {kind: kindGenerate, vision: true, webSearch: true, maxInputTokens: 1_047_576},
	// gpt-4.1-nano has no hosted web_search tool.
	"gpt-4.1-nano": {kind: kindGenerate, vision: true, maxInputTokens: 1_047_576},

	// Embed.
	"text-embedding-3-small": {kind: kindEmbed, dimensions: true, maxInputTokens: 8_192},
	"text-embedding-3-large": {kind: kindEmbed, dimensions: true, maxInputTokens: 8_192},
	"text-embedding-ada-002": {
		kind:       kindEmbed,
		deprecated: true, replacement: "text-embedding-3-small",
		maxInputTokens: 8_192,
	},

	// Image.
	"gpt-image-2": {kind: kindImage},
	"gpt-image-1": {
		kind:       kindImage,
		deprecated: true, replacement: "gpt-image-2",
	},

	// TTS.
	"gpt-4o-mini-tts": {kind: kindTTS},
	"tts-1": {
		kind:       kindTTS,
		deprecated: true, replacement: "gpt-4o-mini-tts",
	},
	"tts-1-hd": {
		kind:       kindTTS,
		deprecated: true, replacement: "gpt-4o-mini-tts",
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog. A custom
// entry replaces the same-named built-in entirely.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, model := range spec.Models {
		models[model.Name] = catalogEntry{
			kind:       modelKind(model.Kind),
			vision:     model.Vision,
			reasoning:  model.Reasoning,
			webSearch:  model.WebSearch,
			dimensions: model.Dimensions,
		}
	}
	return models, nil
}
