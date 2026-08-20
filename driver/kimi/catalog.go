package kimi

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

// catalogEntry declares what one catalog model accepts. capabilities is the
// single capability fact source: input/output content kinds and the reasoning
// control capability. sampling, reasoningEffort, keepThinking, and
// keepThinkingAlways are control capabilities that no capability kind
// expresses and stay separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// sampling accepts the moonshot-v1 sampling knobs (temperature,
	// top_p); the K3 / K2.x request schemas carry none.
	sampling bool
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
// for kimi-k3, kimi-k2.7-code, and kimi-k2.6 (official docs: "均支持文本、
// 图片与视频输入"); kimi-k2.5 and the moonshot-v1 family are documented
// for image understanding only.
var catalog = map[string]catalogEntry{
	"kimi-k3": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways),
		reasoningEffort:    true,
		keepThinkingAlways: true,
		maxInputTokens:     1_000_000,
	},
	"kimi-k2.7-code": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways),
		keepThinkingAlways: true,
		maxInputTokens:     256_000,
	},
	"kimi-k2.7-code-highspeed": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways),
		keepThinkingAlways: true,
		maxInputTokens:     256_000,
	},
	"kimi-k2.6": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		keepThinking:   true,
		maxInputTokens: 256_000,
	},
	"kimi-k2.5": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},

	// moonshot-v1: text generation plus vision previews; the only family
	// with sampling knobs and the only one without thinking.
	"moonshot-v1-8k":                  {kind: kindGenerate, capabilities: generateChatCapabilities(), sampling: true, maxInputTokens: 8_192},
	"moonshot-v1-32k":                 {kind: kindGenerate, capabilities: generateChatCapabilities(), sampling: true, maxInputTokens: 32_768},
	"moonshot-v1-128k":                {kind: kindGenerate, capabilities: generateChatCapabilities(), sampling: true, maxInputTokens: 131_072},
	"moonshot-v1-8k-vision-preview":   {kind: kindGenerate, capabilities: generateChatCapabilities().WithInputs(message.PartImage), sampling: true, maxInputTokens: 8_192},
	"moonshot-v1-32k-vision-preview":  {kind: kindGenerate, capabilities: generateChatCapabilities().WithInputs(message.PartImage), sampling: true, maxInputTokens: 32_768},
	"moonshot-v1-128k-vision-preview": {kind: kindGenerate, capabilities: generateChatCapabilities().WithInputs(message.PartImage), sampling: true, maxInputTokens: 131_072},
}

func (e catalogEntry) validate() error {
	if e.kind != kindGenerate {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	if !slices.Contains(e.capabilities.Outputs, message.PartText) {
		return fmt.Errorf("generate family must declare text output")
	}
	if e.keepThinkingAlways && e.capabilities.Reasoning != inference.ReasoningAlways {
		return fmt.Errorf("always-preserved thinking requires always-on thinking")
	}
	if e.reasoningEffort && e.capabilities.Reasoning == inference.ReasoningNone {
		return fmt.Errorf("reasoning effort requires reasoning")
	}
	return nil
}

// generateChatCapabilities is the capability declaration for the Kimi text
// compiler family: text/data/tool parts in, text out.
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

// mergedCatalog overlays the built-in catalog with the spec's model
// declarations: capability lists union onto the same-named catalog entry,
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
		entry.capabilities.Inputs = unionKinds(
			entry.capabilities.Inputs,
			declared.Capabilities.Inputs,
		)
		entry.capabilities.Outputs = unionKinds(
			entry.capabilities.Outputs,
			declared.Capabilities.Outputs,
		)
		entry.capabilities.HostedWebSearch =
			entry.capabilities.HostedWebSearch || declared.Capabilities.HostedWebSearch
		if declared.Capabilities.Reasoning != inference.ReasoningNone {
			entry.capabilities.Reasoning = declared.Capabilities.Reasoning
		}
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", declared.Name, err)
		}
		models[declared.Name] = entry
	}
	return models, nil
}

// unionKinds appends any kind from addition that is not already present in
// base, preserving base order. Spec declarations overlay catalog entries
// additively.
func unionKinds(base, addition []message.PartKind) []message.PartKind {
	result := append([]message.PartKind(nil), base...)
	for _, kind := range addition {
		if !slices.Contains(result, kind) {
			result = append(result, kind)
		}
	}
	return result
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
