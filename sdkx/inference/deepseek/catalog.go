package deepseek

import (
	"fmt"
	"maps"
	"sort"
)

type modelKind string

const kindGenerate modelKind = "generate"

// catalogEntry declares what one catalog model accepts. The zero value is
// the bare text surface: the compiler rejects every channel the entry
// omits, so a model never silently accepts a feature it may not serve.
type catalogEntry struct {
	kind modelKind
	// reasoning accepts the reasoning effort knob and emits
	// reasoning_content traces.
	reasoning bool
}

// catalog reflects DeepSeek's public API as of 2026-07.
// Sources:
//   - https://api-docs.deepseek.com/quick_start/pricing
//   - https://api-docs.deepseek.com/api/create-chat-completion
//   - https://api-docs.deepseek.com/guides/thinking_mode
//
// The legacy `deepseek-chat` / `deepseek-reasoner` aliases retired on
// 2026-07-24 (https://api-docs.deepseek.com/news/news260424) and are
// deliberately absent: deployments pinning them must migrate to the V4
// names. Both V4 models are hybrid thinking models (thinking enabled by
// default) with a 1M token context and 384K max output.
var catalog = map[string]catalogEntry{
	"deepseek-v4-flash": {kind: kindGenerate, reasoning: true},
	"deepseek-v4-pro":   {kind: kindGenerate, reasoning: true},
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
