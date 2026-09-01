package openai

import (
	"fmt"
	"maps"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind classifies catalog models by the compiler family that serves
// them. It is an implementation discriminator — which wire compiler to bind —
// not a capability declaration: the content kinds a model serves are declared
// explicitly in catalogEntry.capabilities and validated against the family
// contract in validate.
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

// catalogEntry is one model in the built-in catalog. capabilities is the
// single capability fact source: input/output content kinds, hosted web
// search, and the reasoning control capability. dimensions and effortNone
// are control capabilities that no capability kind expresses (embed custom
// output dimensions, and effort "none" to disable reasoning) and stay
// separate flags. ModelSpec mirrors this shape so deployment-declared
// models behave identically.
type catalogEntry struct {
	kind         modelKind
	api          apiMode
	capabilities inference.ModelCapabilities
	// dimensions (embed) allows custom output dimensions. Control
	// capability, likewise not a content kind.
	dimensions bool
	// effortNone marks reasoning models whose reasoning.effort accepts
	// "none" to disable reasoning (gpt-5.1+ per the OpenAI docs); models
	// without it reject a disable request.
	effortNone  bool
	deprecated  bool
	replacement string
	// maxInputTokens caps the input context (prompt plus prior turns) in
	// tokens; zero means undeclared. Generate values mirror the context
	// window on https://developers.openai.com/api/docs/models; embedding
	// values mirror the per-request input limit.
	maxInputTokens int
}

// validate enforces the family contract: the compiler bound by kind can only
// serve the output modalities it produces, so kind and capabilities cannot
// drift.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	switch e.kind {
	case kindGenerate:
		if !slices.Contains(e.capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !slices.Contains(e.capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindTTS:
		if !slices.Contains(e.capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("tts family must declare audio output")
		}
	case kindEmbed:
		if len(e.capabilities.Outputs) != 0 {
			return fmt.Errorf("embed family declares no generate output")
		}
	}
	return nil
}

// generateChatCapabilities is the common capability declaration for the
// text chat/responses compiler family. Individual entries add image input
// when the model has vision and the reasoning kind when the model reasons;
// hosted web search rides on the capabilities bit.
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

// openaiEffortMap is the canonical-to-wire identity map: OpenAI's
// reasoning.effort accepts the canonical five verbatim.
var openaiEffortMap = map[inference.ReasoningEffort]string{
	inference.ReasoningMinimal: string(inference.ReasoningMinimal),
	inference.ReasoningLow:     string(inference.ReasoningLow),
	inference.ReasoningMedium:  string(inference.ReasoningMedium),
	inference.ReasoningHigh:    string(inference.ReasoningHigh),
	inference.ReasoningXHigh:   string(inference.ReasoningXHigh),
}

// catalog is the built-in model list, aligned with the OpenAI model lineup
// of July 2026 (GPT-5.6 family flagship). Deployments extend or override it
// via Spec.Models.
var catalog = map[string]catalogEntry{
	// Generate — GPT-5.6 flagship family (reasoning + vision).
	"gpt-5.6-sol": {
		kind:           kindGenerate,
		capabilities:   generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		effortNone:     true,
		maxInputTokens: 1_050_000,
	},
	"gpt-5.6-terra": {
		kind:           kindGenerate,
		capabilities:   generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		effortNone:     true,
		maxInputTokens: 1_050_000,
	},
	"gpt-5.6-luna": {
		kind:           kindGenerate,
		capabilities:   generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		effortNone:     true,
		maxInputTokens: 1_050_000,
	},
	// Generate — previous generations, superseded but available.
	"gpt-5.5": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-sol",
		maxInputTokens: 1_050_000,
	},
	"gpt-5.4": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-sol",
		maxInputTokens: 1_050_000,
	},
	"gpt-5.4-mini": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-terra",
		maxInputTokens: 400_000,
	},
	"gpt-5.4-nano": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-luna",
		maxInputTokens: 400_000,
	},
	// Generate — GPT-4.1 line: vision without the reasoning control.
	"gpt-4.1": {
		kind:           kindGenerate,
		capabilities:   generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch(),
		maxInputTokens: 1_047_576,
	},
	"gpt-4.1-mini": {
		kind:           kindGenerate,
		capabilities:   generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch(),
		maxInputTokens: 1_047_576,
	},
	// gpt-4.1-nano has no hosted web_search tool.
	"gpt-4.1-nano": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage),
		deprecated:   true, replacement: "gpt-5.6-luna",
		maxInputTokens: 1_047_576,
	},

	// Embed.
	"text-embedding-3-small": {kind: kindEmbed, dimensions: true, maxInputTokens: 8_192},
	"text-embedding-3-large": {kind: kindEmbed, dimensions: true, maxInputTokens: 8_192},
	"text-embedding-ada-002": {
		kind:       kindEmbed,
		deprecated: true, replacement: "text-embedding-3-small",
		maxInputTokens: 8_192,
	},

	// Image.
	"gpt-image-2": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"gpt-image-1": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartImage),
		deprecated: true, replacement: "gpt-image-2",
	},

	// TTS.
	"gpt-4o-mini-tts": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"tts-1": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
		deprecated: true, replacement: "gpt-4o-mini-tts",
	},
	"tts-1-hd": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
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
			kind:         modelKind(model.Kind),
			capabilities: model.Capabilities,
			dimensions:   model.Dimensions,
			effortNone:   model.EffortNone,
		}
	}
	for name, entry := range models {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
	}
	return models, nil
}
