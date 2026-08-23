package bytedance

import (
	"fmt"
	"maps"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind groups models by the service family that serves them. The kind,
// not the model name, selects the compiler and transport.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindVideo    modelKind = "video"
)

// catalogEntry is one model's declared capability set. capabilities is the
// single capability fact source: input/output content kinds, hosted web
// search, and the reasoning control capability. dimensions and maxResolution
// are control capabilities that no capability kind expresses and stay
// separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// embed: accepts custom output dimensions.
	dimensions bool
	// video: highest supported resolution tier ("720p", "1080p", "4k");
	// empty leaves resolution unconstrained.
	maxResolution string
	// lifecycle: deprecated models stay routable but announce a replacement.
	deprecated  bool
	replacement string
	// maxInputTokens caps the input context in tokens; zero means
	// undeclared. Generate values mirror the family context window on
	// the Volcengine Ark model detail pages; the official model list
	// (https://www.volcengine.com/docs/82379/1330310) reports separate
	// per-version max-input values (typically 224K or 256K, with 1M
	// long-context tiers), so these entries are the family-level upper
	// bound. Embedding values mirror the documented per-input limit.
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
	case kindVideo:
		if !slices.Contains(e.capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	case kindEmbed:
		if len(e.capabilities.Outputs) != 0 {
			return fmt.Errorf("%s family declares no generate output", e.kind)
		}
	}
	return nil
}

// generateChatCapabilities is the common capability declaration for the Ark
// text compiler family. Individual entries add image/video input, hosted web
// search, and the reasoning control capability. Ark consumes no reasoning
// input, so PartReasoning is deliberately absent.
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

// catalog is the built-in model registry. Names are stable Volcengine model
// families; deployment-specific endpoint IDs (ep-xxx or dated revisions like
// doubao-seed-2-1-pro-260628) belong in Spec.Endpoints.
//
// Unknown models fail closed: the factory only exposes models listed here or
// declared via Spec.Models.
var catalog = map[string]catalogEntry{
	// Doubao Seed Evolving (2026-07) — rapidly iterating Coding & Agent
	// line, updated weekly. Long 1M context; natively multimodal with
	// thinking and the full hosted tool surface.
	"doubao-seed-evolving": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 1_024_000,
	},

	// Seed 2.1 (2026-06) — current flagship tier. The whole Seed 2.x line is
	// natively multimodal (text/image/video in) with thinking support.
	"doubao-seed-2-1-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},
	"doubao-seed-2-1-turbo": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},

	// Seed 2.0 (2026-02) — general agent line plus the code-tuned variant.
	// The 260215 revisions retired in 2026-05, but the 260428 lite/mini
	// revisions stay live and are Ark's recommended audio-understanding
	// models, so the stable family names remain routable.
	"doubao-seed-2-0-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},
	"doubao-seed-2-0-lite": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},
	"doubao-seed-2-0-mini": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},
	"doubao-seed-2-0-code": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 256_000,
	},

	// Seed 1.x — superseded by the 2.x line; kept routable for existing
	// deployments, announced as deprecated with a replacement.
	"doubao-seed-1-8": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		deprecated: true, replacement: "doubao-seed-2-0-lite",
		maxInputTokens: 256_000,
	},
	"doubao-seed-1-6-vision": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle),
		deprecated: true, replacement: "doubao-seed-2-0-lite",
		maxInputTokens: 256_000,
	},

	"doubao-embedding-large": {
		kind:           kindEmbed,
		capabilities:   inference.ModelCapabilities{}.WithInputs(message.PartText),
		dimensions:     true,
		maxInputTokens: 4_095,
	},
	"doubao-embedding-vision": {
		kind: kindEmbed,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage),
		dimensions:     true,
		maxInputTokens: 8_191,
	},

	// Seedream image generation; 5.0-pro/5.0/4.5 current, 4.0 superseded.
	"doubao-seedream-5-0-pro": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-5-0": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-4-5": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-4-0": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
		deprecated: true, replacement: "doubao-seedream-5-0",
	},

	// Seedance video generation, served by the async content-generation task
	// API behind the unary contract. 2.0 is the current line; fast/mini cap
	// at 720p. 2.5 is the current flagship (1080p, 30s narrative). 1.x
	// stays routable for existing deployments.
	"doubao-seedance-2-5": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "1080p",
	},
	"doubao-seedance-2-0": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "4k",
	},
	"doubao-seedance-2-0-fast": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
	},
	"doubao-seedance-2-0-mini": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
	},
	"doubao-seedance-1-5-pro": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "1080p",
		deprecated:    true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-pro": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "1080p",
		deprecated:    true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-lite-t2v": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
		deprecated:    true, replacement: "doubao-seedance-2-0-fast",
	},
	"doubao-seedance-1-0-lite-i2v": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
		deprecated:    true, replacement: "doubao-seedance-2-0-fast",
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog and returns
// the merged view. Spec entries replace catalog entries by name.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(merged, catalog)
	for _, model := range spec.Models {
		merged[model.Name] = catalogEntry{
			kind:          modelKind(model.Kind),
			capabilities:  model.Capabilities,
			dimensions:    model.Dimensions,
			maxResolution: model.MaxResolution,
		}
	}
	for name, entry := range merged {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
	}
	return merged, nil
}
