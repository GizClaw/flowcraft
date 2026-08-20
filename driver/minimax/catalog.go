package minimax

import (
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
	kindVideo    modelKind = "video"
	kindMusic    modelKind = "music"
)

// catalogEntry declares what one catalog model accepts. capabilities is the
// single capability fact source: input/output content kinds and the reasoning
// control capability. video10s, videoHD, and videoI2VOnly are control
// capabilities that no capability kind expresses and stay separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// video10s accepts 10-second durations at 768P.
	video10s bool
	// videoHD accepts 1080P resolution.
	videoHD bool
	// videoI2VOnly marks image-to-video models: the request must carry a
	// first-frame image.
	videoI2VOnly bool
	// maxInputTokens caps the input context in tokens; zero means
	// undeclared. M3 holds the 1M context and the M2.x series holds
	// 204,800 per https://platform.minimaxi.com/docs/guides/text-generation.
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
	case kindTTS, kindMusic:
		if !slices.Contains(e.capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("%s family must declare audio output", e.kind)
		}
	case kindVideo:
		if !slices.Contains(e.capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	default:
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	return nil
}

// generateChatCapabilities is the common capability declaration for the
// MiniMax Messages compiler family. Individual entries add image input when
// the model has vision and the reasoning kind the model serves.
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

// catalog reflects MiniMax's lineup as of 2026-07. Sources:
//   - https://platform.minimaxi.com/docs/api-reference/api-overview
//   - https://platform.minimaxi.com/docs/api-reference/speech-t2a-http
//   - https://platform.minimaxi.com/docs/api-reference/video-generation-t2v
//   - https://platform.minimaxi.com/docs/api-reference/image-generation-t2i
//
// All generate entries speak the binary-thinking dialect: any requested
// reasoning effort compiles to thinking: {type: "adaptive"} — the endpoint
// has no effort levels. MiniMax-M3 holds a 1M token context; the M2.x
// series holds 204,800. Music generation (music-3.0) serves the canonical
// audio intent with lyrics/format through MusicOptions; music-cover stays
// out because it has no honest surface in that intent.
var catalog = map[string]catalogEntry{
	"MiniMax-M3": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		maxInputTokens: 1_000_000,
	},
	"MiniMax-M2.7": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2.7-highspeed": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2.5": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2.5-highspeed": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2.1": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2.1-highspeed": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},
	"MiniMax-M2": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningAlways),
		maxInputTokens: 204_800,
	},

	// Speech synthesis (t2a_v2): HD and turbo tiers.
	"speech-2.8-hd": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"speech-2.8-turbo": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"speech-2.6-hd": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"speech-2.6-turbo": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"speech-02-hd": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"speech-02-turbo": {
		kind: kindTTS,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},

	// Image generation.
	"image-01": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"image-01-live": {
		kind: kindImage,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},

	// Video generation (async task API). Hailuo-2.3-Fast is image-to-video
	// only; the 2.3/02 pair runs 10s at 768P and 6s at 1080P.
	"MiniMax-Hailuo-2.3": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		video10s: true,
		videoHD:  true,
	},
	"MiniMax-Hailuo-2.3-Fast": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		videoI2VOnly: true,
	},
	"MiniMax-Hailuo-02": {
		kind: kindVideo,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		video10s: true,
		videoHD:  true,
	},

	// Music generation (text-to-music; music-cover stays out — see
	// music.go). The -free tiers are rate-limited gratis twins.
	"music-3.0": {
		kind: kindMusic,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"music-3.0-free": {
		kind: kindMusic,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"music-2.6": {
		kind: kindMusic,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	},
	"music-2.6-free": {
		kind: kindMusic,
		capabilities: inference.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
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
