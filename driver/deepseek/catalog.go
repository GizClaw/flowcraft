package deepseek

import (
	"fmt"
	"maps"
	"sort"
)

type modelKind string

const kindGenerate modelKind = "generate"

// apiMode selects the generate wire surface for one provider instance.
type apiMode string

const (
	apiChat      apiMode = "chat"
	apiResponses apiMode = "responses"
)

// catalogEntry declares what one catalog model accepts. The zero value is
// the bare text surface: the compiler rejects every channel the entry
// omits, so a model never silently accepts a feature it may not serve.
type catalogEntry struct {
	kind modelKind
	// api is the provider-level generate surface selected by Spec.API.
	api apiMode
	// declared marks a Spec.Models entry (as opposed to a built-in catalog
	// model). Responses-mode filtering treats declared entries fail-fast.
	declared bool
	// reasoning accepts the thinking/effort knobs and emits reasoning
	// traces.
	reasoning bool
	// responses accepts the Responses API surface. Chat completions work
	// for every catalog model; responses is per-model.
	responses bool
	// webSearch accepts the hosted web_search tool on the Responses API.
	webSearch bool
	// maxInputTokens caps the input context in tokens; zero means
	// undeclared. Both V4 models carry the 1M context published on
	// https://api-docs.deepseek.com/quick_start/pricing.
	maxInputTokens int
}

// catalog reflects DeepSeek's public API as of 2026-08.
// Sources:
//   - https://api-docs.deepseek.com/quick_start/pricing
//   - https://api-docs.deepseek.com/guides/responses_api
//   - https://api-docs.deepseek.com/guides/thinking_mode
//
// The legacy `deepseek-chat` / `deepseek-reasoner` aliases retired on
// 2026-07-24 and are deliberately absent. Both V4 models are hybrid
// thinking models (thinking enabled by default) with a 1M token context.
// The Responses API serves both V4 models; deepseek-v4-pro support
// landed after the initial flash-only launch.
var catalog = map[string]catalogEntry{
	"deepseek-v4-flash": {
		kind:           kindGenerate,
		reasoning:      true,
		responses:      true,
		webSearch:      true,
		maxInputTokens: 1_000_000,
	},
	"deepseek-v4-pro": {
		kind:           kindGenerate,
		reasoning:      true,
		responses:      true,
		webSearch:      true,
		maxInputTokens: 1_000_000,
	},
}

// mergedCatalog overlays the built-in catalog with the spec's model
// declarations: a spec entry with a catalog name replaces that entry, and
// unknown names extend the catalog. Models stay fail closed — the factory
// only exposes what the merged catalog declares.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, declared := range spec.Models {
		entry := catalogEntry{
			kind:      modelKind(declared.Kind),
			reasoning: declared.Reasoning,
			responses: declared.Responses,
			webSearch: declared.WebSearch,
			declared:  true,
		}
		if entry.kind == "" {
			if existing, exists := models[declared.Name]; exists {
				entry.kind = existing.kind
			} else {
				entry.kind = kindGenerate
			}
		}
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", declared.Name, err)
		}
		models[declared.Name] = entry
	}
	return models, nil
}

func (e catalogEntry) validate() error {
	if e.kind != kindGenerate {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	return nil
}

// sortedNames returns catalog names in deterministic order so factory
// output is stable across runs.
func sortedNames(models map[string]catalogEntry) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
