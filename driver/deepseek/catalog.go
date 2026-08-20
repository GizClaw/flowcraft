package deepseek

import (
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

type modelKind string

const kindGenerate modelKind = "generate"

// apiMode selects the generate wire surface for one provider instance.
type apiMode string

const (
	apiChat      apiMode = "chat"
	apiResponses apiMode = "responses"
)

// catalogEntry declares what one catalog model accepts. capabilities is the
// single capability fact source: input/output content kinds, hosted web
// search, and the reasoning control capability. api, declared, and responses
// are wire-surface facts, not content capabilities, and stay separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// api is the provider-level generate surface selected by Spec.API.
	api apiMode
	// declared marks a Spec.Models entry (as opposed to a built-in catalog
	// model). Responses-mode filtering treats declared entries fail-fast.
	declared bool
	// responses accepts the Responses API surface. Chat completions work
	// for every catalog model; responses is per-model.
	responses bool
	// maxInputTokens caps the input context in tokens; zero means
	// undeclared. Both V4 models carry the 1M context published on
	// https://api-docs.deepseek.com/quick_start/pricing.
	maxInputTokens int
}

// validate enforces the generate family contract: the compiler only serves
// text output, so kind and capabilities cannot drift.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	if e.kind != kindGenerate {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	if !slices.Contains(e.capabilities.Outputs, message.PartText) {
		return fmt.Errorf("generate family must declare text output")
	}
	return nil
}

// generateChatCapabilities is the capability declaration for the DeepSeek
// text compiler family: text/data/tool parts in, text out. DeepSeek
// consumes no image/audio/video input.
func generateChatCapabilities() inference.ModelCapabilities {
	return inference.ModelCapabilities{
		Inputs: []message.PartKind{
			message.PartText,
			message.PartData,
			message.PartToolCall,
			message.PartToolResult,
		},
		Outputs: []message.PartKind{message.PartText},
	}
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
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		responses:      true,
		maxInputTokens: 1_000_000,
	},
	"deepseek-v4-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		responses:      true,
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
			kind:         modelKind(declared.Kind),
			capabilities: declared.Capabilities,
			responses:    declared.Responses,
			declared:     true,
		}
		if entry.kind == "" {
			if existing, exists := models[declared.Name]; exists {
				entry.kind = existing.kind
			} else {
				entry.kind = kindGenerate
			}
		}
		models[declared.Name] = entry
	}
	for name, entry := range models {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", name, err)
		}
	}
	return models, nil
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
