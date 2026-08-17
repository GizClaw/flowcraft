package kimi

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
	// vision accepts image input parts (URL or Base64 data URI).
	vision bool
	// video accepts video input parts (URL or Base64 data URI).
	video bool
	// sampling accepts the moonshot-v1 sampling knobs (temperature,
	// top_p); the K3 / K2.x request schemas carry none.
	sampling bool
	// reasoning accepts the thinking control (enabled/disabled toggle, or
	// the always-on k3 effort dial) and emits reasoning_content traces.
	reasoning bool
	// reasoningAlways marks models whose thinking cannot be switched off:
	// ReasoningEnabled=false rejects (kimi-k3, kimi-k2.7-code).
	reasoningAlways bool
	// reasoningEffort marks models with the top-level reasoning_effort
	// dial (kimi-k3 only); elsewhere an explicit effort drops with a
	// reason.
	reasoningEffort bool
	// keepThinking marks models that optionally re-ingest history
	// reasoning_content via thinking.keep="all" (kimi-k2.6).
	keepThinking bool
	// keepThinkingAlways marks models that always preserve history
	// reasoning (kimi-k3, kimi-k2.7-code): traces round-trip natively and
	// no knob exists to turn the behaviour off.
	keepThinkingAlways bool
	// maxInputTokens caps the input context in tokens; zero means
	// undeclared. Values mirror the context windows published on
	// https://platform.kimi.com/docs/models (moonshot-v1 variants state
	// 8k/32k/128k).
	maxInputTokens int
}

// catalog reflects Kimi's public API as of 2026-07.
// Sources:
//   - https://platform.kimi.com/docs/models
//   - https://platform.kimi.com/docs/api/chat
//
// The retired kimi-k2 series (offline 2026-05-25) and kimi-latest /
// kimi-thinking-preview are deliberately absent. Video input is declared
// for kimi-k3 only: the K2.x multimodal entries are documented for image
// understanding, and video comprehension is a k3 capability.
var catalog = map[string]catalogEntry{
	"kimi-k3": {
		kind:               kindGenerate,
		vision:             true,
		video:              true,
		reasoning:          true,
		reasoningAlways:    true,
		reasoningEffort:    true,
		keepThinkingAlways: true,
		maxInputTokens:     1_000_000,
	},
	"kimi-k2.7-code": {
		kind:               kindGenerate,
		vision:             true,
		reasoning:          true,
		reasoningAlways:    true,
		keepThinkingAlways: true,
		maxInputTokens:     256_000,
	},
	"kimi-k2.7-code-highspeed": {
		kind:               kindGenerate,
		vision:             true,
		reasoning:          true,
		reasoningAlways:    true,
		keepThinkingAlways: true,
		maxInputTokens:     256_000,
	},
	"kimi-k2.6": {
		kind:           kindGenerate,
		vision:         true,
		reasoning:      true,
		keepThinking:   true,
		maxInputTokens: 256_000,
	},
	"kimi-k2.5": {
		kind:           kindGenerate,
		vision:         true,
		reasoning:      true,
		maxInputTokens: 256_000,
	},

	// moonshot-v1: text generation plus vision previews; the only family
	// with sampling knobs and the only one without thinking.
	"moonshot-v1-8k":                  {kind: kindGenerate, sampling: true, maxInputTokens: 8_192},
	"moonshot-v1-32k":                 {kind: kindGenerate, sampling: true, maxInputTokens: 32_768},
	"moonshot-v1-128k":                {kind: kindGenerate, sampling: true, maxInputTokens: 131_072},
	"moonshot-v1-8k-vision-preview":   {kind: kindGenerate, sampling: true, vision: true, maxInputTokens: 8_192},
	"moonshot-v1-32k-vision-preview":  {kind: kindGenerate, sampling: true, vision: true, maxInputTokens: 32_768},
	"moonshot-v1-128k-vision-preview": {kind: kindGenerate, sampling: true, vision: true, maxInputTokens: 131_072},
}

func (e catalogEntry) validate() error {
	if e.kind != kindGenerate {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	if e.keepThinkingAlways && !e.reasoningAlways {
		return fmt.Errorf("always-preserved thinking requires always-on thinking")
	}
	if e.reasoningEffort && !e.reasoning {
		return fmt.Errorf("reasoning effort requires reasoning")
	}
	return nil
}

// mergedCatalog overlays the built-in catalog with the spec's model
// declarations: capability flags OR onto the same-named catalog entry,
// and unknown names extend the catalog as bare generate models.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, declared := range spec.Models {
		if entry, exists := models[declared.Name]; exists {
			if declared.Kind != "" && modelKind(declared.Kind) != entry.kind {
				return nil, fmt.Errorf("model %q kind %q conflicts with catalog %q",
					declared.Name, declared.Kind, entry.kind)
			}
		}
		entry := models[declared.Name]
		if entry.kind == "" {
			entry.kind = kindGenerate
		}
		entry.vision = entry.vision || declared.Vision
		entry.video = entry.video || declared.Video
		entry.reasoning = entry.reasoning || declared.Reasoning
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", declared.Name, err)
		}
		models[declared.Name] = entry
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
