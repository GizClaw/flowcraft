package qwen

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
)

// catalogEntry declares what one catalog model accepts. capabilities is the
// single capability fact source: input/output content kinds, the reasoning
// control capability (switch kind plus the canonical-to-wire effort map),
// and custom embed output dimensions. preserveThinking, thinkingStreamOnly,
// and embedDimensions are control facts that no capability kind expresses
// and stay separate fields; embedDimensions additionally carries the exact
// sizes the published custom_embed_dimensions capability allows and must
// agree with it (validated in catalogEntry.validate).
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// preserveThinking can re-ingest reasoning_content history
	// (preserve_thinking); models without it drop round-trip traces.
	preserveThinking bool
	// thinkingStreamOnly marks models whose thinking mode answers on SSE
	// only: a unary compile with thinking on rejects the unary shape.
	thinkingStreamOnly bool
	// embedDimensions lists the vector sizes an embed model accepts; nil
	// means the model takes no dimension parameter.
	embedDimensions []int
	// limits carries the model's context/output windows in tokens. Nil
	// leaves are undeclared. Values mirror the published maximum input
	// length (最大输入长度) and output length (最大输出长度) on the
	// per-model pages at
	// https://www.alibabacloud.com/help/zh/model-studio/models.
	limits inference.ModelLimits
}

// qwenMaxPreviewEffortMap is qwen3.8-max-preview's canonical-to-wire map
// per the DashScope docs (low/medium/xhigh): canonical minimal folds onto
// low and canonical high reaches the top level "xhigh".
var qwenMaxPreviewEffortMap = map[inference.ReasoningEffort]string{
	inference.ReasoningMinimal: string(inference.ReasoningLow),
	inference.ReasoningLow:     string(inference.ReasoningLow),
	inference.ReasoningMedium:  string(inference.ReasoningMedium),
	inference.ReasoningHigh:    string(inference.ReasoningXHigh),
	inference.ReasoningXHigh:   string(inference.ReasoningXHigh),
}

// catalog reflects the DashScope commercial lineup as of 2026-07.
// Sources:
//   - https://www.alibabacloud.com/help/zh/model-studio/qwen-api-reference
//   - https://www.alibabacloud.com/help/zh/model-studio/models
//
// The qwen3.7/qwen3.8 commercial models are multimodal and hybrid-thinking;
// qwen3.8-max-preview is thinking-only (DashScope rejects enable_thinking
// false with a 400), while the rest can toggle. Thinking mode is stream-only
// server-side, so unary compiles with thinking on reject the shape. The
// legacy qwen-plus/turbo/flash/max names stay text-only here — custom models
// declare through the spec.
var catalog = map[string]catalogEntry{
	"qwen3.8-max-preview": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningAlways).
			WithReasoningEffortMap(qwenMaxPreviewEffortMap),
		preserveThinking:   true,
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(983_616).
			WithMaxOutputTokens(131_072),
	},
	"qwen3.7-max": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		preserveThinking:   true,
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(991_808).
			WithMaxOutputTokens(131_072),
	},
	"qwen3.7-plus": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		preserveThinking:   true,
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(991_808).
			WithMaxOutputTokens(131_072),
	},
	"qwen3.7-flash": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		preserveThinking:   true,
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(991_808).
			WithMaxOutputTokens(131_072),
	},
	"qwen3-vl-plus": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(260_096).
			WithMaxOutputTokens(32_768),
	},
	"qwen3-vl-flash": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithReasoning(inference.ReasoningToggle),
		thinkingStreamOnly: true,
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(260_096).
			WithMaxOutputTokens(32_768),
	},

	"qwen-plus": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(997_952).
			WithMaxOutputTokens(32_768),
	},
	"qwen-turbo": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(98_304).
			WithMaxOutputTokens(16_384),
	},
	"qwen-flash": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(997_952).
			WithMaxOutputTokens(32_768),
	},
	"qwen-max": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(30_720).
			WithMaxOutputTokens(8_192),
	},

	// Embeddings. The multimodal model is served in the Beijing region
	// only; text-embedding-v4 batches at most 10 rows per request.
	"text-embedding-v4": {
		kind: kindEmbed,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithCustomEmbedDimensions(),
		embedDimensions: []int{2048, 1536, 1024, 768, 512, 256, 128, 64},
		limits:          inference.ModelLimits{}.WithMaxInputTokens(8_192),
	},
	"qwen3-vl-embedding": {
		kind: kindEmbed,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage, message.PartVideo).
			WithCustomEmbedDimensions(),
		embedDimensions: []int{2560, 2048, 1536, 1024, 768, 512, 256},
		limits:          inference.ModelLimits{}.WithMaxInputTokens(32_000),
	},
}

func (e catalogEntry) validate() error {
	if e.kind != kindGenerate && e.kind != kindEmbed {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	if e.kind == kindGenerate && !slices.Contains(e.capabilities.Outputs, message.PartText) {
		return fmt.Errorf("generate family must declare text output")
	}
	if e.kind == kindEmbed && len(e.capabilities.Outputs) != 0 {
		return fmt.Errorf("embed family declares no generate output")
	}
	if e.kind == kindEmbed && e.capabilities.Reasoning.Kind != inference.ReasoningNone {
		return fmt.Errorf("embed model cannot declare reasoning")
	}
	if e.kind == kindEmbed && (e.preserveThinking || e.thinkingStreamOnly) {
		return fmt.Errorf("embed model cannot declare thinking flags")
	}
	if e.kind == kindEmbed &&
		(len(e.embedDimensions) > 0) != e.capabilities.CustomEmbedDimensions {
		return fmt.Errorf(
			"embed model custom_embed_dimensions capability must match its "+
				"dimension whitelist (%d sizes)",
			len(e.embedDimensions),
		)
	}
	return e.limits.Validate()
}

// generateChatCapabilities is the capability declaration for the Qwen text
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

// multimodal reports whether the model rides the multimodal-generation
// endpoint rather than text-generation.
func (e catalogEntry) multimodal() bool {
	return slices.Contains(e.capabilities.Inputs, message.PartImage) ||
		slices.Contains(e.capabilities.Inputs, message.PartVideo)
}

// mergedCatalog overlays the spec's declared models on the built-in
// catalog. Unknown kinds are rejected at build time; custom models get the
// bare declared surface (fail closed).
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	for name, entry := range catalog {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
		merged[name] = entry
	}
	for _, model := range spec.Models {
		if entry, exists := merged[model.Name]; exists {
			if model.Kind != "" && modelKind(model.Kind) != entry.kind {
				return nil, fmt.Errorf("model %q kind %q conflicts with catalog %q",
					model.Name, model.Kind, entry.kind)
			}
		}
		entry := merged[model.Name]
		if entry.kind == "" {
			entry.kind = modelKind(model.Kind)
			if entry.kind == "" {
				entry.kind = kindGenerate
			}
		}
		entry.capabilities.Inputs = unionKinds(
			entry.capabilities.Inputs,
			model.Capabilities.Inputs,
		)
		entry.capabilities.Outputs = unionKinds(
			entry.capabilities.Outputs,
			model.Capabilities.Outputs,
		)
		entry.capabilities.HostedWebSearch =
			entry.capabilities.HostedWebSearch || model.Capabilities.HostedWebSearch
		if model.Capabilities.Reasoning.Kind != inference.ReasoningNone {
			entry.capabilities.Reasoning = model.Capabilities.Reasoning
		}
		if model.Limits.MaxInputTokens != nil {
			value := *model.Limits.MaxInputTokens
			entry.limits.MaxInputTokens = &value
		}
		if model.Limits.MaxOutputTokens != nil {
			value := *model.Limits.MaxOutputTokens
			entry.limits.MaxOutputTokens = &value
		}
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", model.Name, err)
		}
		merged[model.Name] = entry
	}
	return merged, nil
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
