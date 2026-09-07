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
// control capability (switch kind plus the canonical-to-wire effort map).
// sampling, keepThinking, and keepThinkingAlways are control capabilities
// that no capability kind expresses and stay separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// sampling accepts the moonshot-v1 sampling knobs (temperature,
	// top_p); the K3 / K2.x request schemas carry none.
	sampling bool
	// keepThinking marks models that optionally re-ingest history
	// reasoning_content via thinking.keep="all" (kimi-k2.6).
	keepThinking bool
	// keepThinkingAlways marks models that always preserve history
	// reasoning (kimi-k3, kimi-k2.7-code): traces round-trip natively and
	// no knob exists to turn the behaviour off.
	keepThinkingAlways bool
	// limits carries the model's context/output windows in tokens. Nil
	// leaves are undeclared. Values mirror the context windows published
	// on https://platform.kimi.com/docs/models (moonshot-v1 variants state
	// 8k/32k/128k); kimi-k3's output ceiling comes from the official
	// quickstart (max_completion_tokens up to 1048576,
	// https://platform.kimi.com/docs/guide/kimi-k3-quickstart).
	limits inference.ModelLimits
}

// kimiK3EffortMap is kimi-k3's canonical-to-wire effort map per the Kimi
// docs (low/high/max): canonical minimal and medium fold onto low/high,
// and xhigh reaches the model's top level "max".
var kimiK3EffortMap = map[inference.ReasoningEffort]string{
	inference.ReasoningMinimal: string(inference.ReasoningLow),
	inference.ReasoningLow:     string(inference.ReasoningLow),
	inference.ReasoningMedium:  string(inference.ReasoningHigh),
	inference.ReasoningHigh:    string(inference.ReasoningHigh),
	inference.ReasoningXHigh:   "max",
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
			WithReasoning(inference.ReasoningAlways).
			WithReasoningEffortMap(kimiK3EffortMap),
		keepThinkingAlways: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(1_048_576),
	},
	"kimi-k2.7-code": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways),
		keepThinkingAlways: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(131_072),
	},
	"kimi-k2.7-code-highspeed": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways),
		keepThinkingAlways: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(131_072),
	},
	"kimi-k2.6": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		keepThinking: true,
		limits:       inference.ModelLimits{}.WithMaxInputTokens(256_000),
	},
	"kimi-k2.5": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		limits: inference.ModelLimits{}.WithMaxInputTokens(256_000),
	},

	// moonshot-v1: text generation plus vision previews; the only family
	// with sampling knobs and the only one without thinking.
	"moonshot-v1-8k": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		sampling:     true,
		limits:       inference.ModelLimits{}.WithMaxInputTokens(8_192),
	},
	"moonshot-v1-32k": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		sampling:     true,
		limits:       inference.ModelLimits{}.WithMaxInputTokens(32_768),
	},
	"moonshot-v1-128k": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		sampling:     true,
		limits:       inference.ModelLimits{}.WithMaxInputTokens(131_072),
	},
	"moonshot-v1-8k-vision-preview": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage),
		sampling: true,
		limits:   inference.ModelLimits{}.WithMaxInputTokens(8_192),
	},
	"moonshot-v1-32k-vision-preview": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage),
		sampling: true,
		limits:   inference.ModelLimits{}.WithMaxInputTokens(32_768),
	},
	"moonshot-v1-128k-vision-preview": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage),
		sampling: true,
		limits:   inference.ModelLimits{}.WithMaxInputTokens(131_072),
	},
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
	if e.keepThinkingAlways && e.capabilities.Reasoning.Kind != inference.ReasoningAlways {
		return fmt.Errorf("always-preserved thinking requires always-on thinking")
	}
	return e.limits.Validate()
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

// mergedCatalog overlays the spec's declared models on the built-in
// catalog: a spec entry that names a catalog entry is a leaf patch over it
// (capability leaves and numeric limits are inherited unless declared),
// and unknown names start from the conservative zero declaration. Models
// stay fail closed — the factory only exposes what the merged catalog
// declares.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, declared := range spec.Models {
		entry := catalogEntry{kind: modelKind(declared.Kind)}
		base := inference.ModelCapabilities{}
		if existing, exists := models[declared.Name]; exists {
			if entry.kind == "" {
				entry.kind = existing.kind
			}
			if entry.kind == existing.kind {
				// Same-kind redeclarations keep the catalog's driver
				// control facts and limits; capability leaves the
				// declaration does not name are inherited too.
				entry = existing
				base = existing.capabilities
			}
		} else {
			if entry.kind == "" {
				entry.kind = kindGenerate
			}
		}
		entry.capabilities = declared.Capabilities.Apply(base)
		if declared.Limits.MaxInputTokens != nil {
			value := *declared.Limits.MaxInputTokens
			entry.limits.MaxInputTokens = &value
		}
		if declared.Limits.MaxOutputTokens != nil {
			value := *declared.Limits.MaxOutputTokens
			entry.limits.MaxOutputTokens = &value
		}
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

// descriptorFor lowers one catalog entry into its public discovery
// descriptor under id. buildProvider and Catalog share this lowering so
// offline catalog views cannot drift from deployed provider models.
func descriptorFor(id inference.ModelID, entry catalogEntry) inference.ModelDescriptor {
	descriptor := inference.ModelDescriptor{
		ID:           id,
		Capabilities: entry.capabilities,
	}
	descriptor.Limits = entry.limits.Clone()
	return descriptor
}

// Catalog returns every built-in model descriptor under provider, sorted by
// model name. Descriptors carry the driver-declared capabilities, limits, and
// lifecycle; Operations stay empty because the inference assembly derives them
// from the model's openers after deployment. Provider IDs are a deployment
// property, so callers supply the identity that appears in each descriptor.
func Catalog(provider string) ([]inference.ModelDescriptor, error) {
	if provider == "" {
		return nil, fmt.Errorf("catalog: provider is required")
	}
	descriptors := make([]inference.ModelDescriptor, 0, len(catalog))
	for _, name := range sortedNames(catalog) {
		descriptor := descriptorFor(
			inference.ModelID{Provider: provider, Name: name},
			catalog[name],
		)
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}
