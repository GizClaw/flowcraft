package anthropic

import (
	"fmt"
	"maps"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// catalogEntry describes one model's compile-time capabilities.
// capabilities is the single capability fact source: input/output content
// kinds and the reasoning control capability. efforts is the model's
// private reasoning-effort dial: the wire tokens the thinking endpoint
// accepts, ordered as an ascending depth ladder.
type catalogEntry struct {
	capabilities inference.ModelCapabilities
	// efforts lists the effort tokens this model's thinking endpoint
	// accepts (low/medium/high/xhigh/max per the Claude docs); nil means
	// binary thinking at platform-chosen depth.
	efforts     []inference.ReasoningEffort
	deprecated  bool
	replacement string
	// maxInputTokens caps the input context (system + messages + tools)
	// in tokens; zero means undeclared. Values mirror the context window
	// published on https://platform.claude.com/docs/en/about-claude/models.
	maxInputTokens int
}

// validate enforces the generate family contract: Claude compilers only
// serve text output.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	if !slices.Contains(e.capabilities.Outputs, message.PartText) {
		return fmt.Errorf("generate family must declare text output")
	}
	return nil
}

// generateChatCapabilities is the common capability declaration for the
// Claude text compiler family.
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

// claudeEfforts is the effort dial shared by every Claude model that
// accepts effort levels, aligned with the adaptive-thinking effort
// parameter (low/medium/high/xhigh/max). "max" is a model-side extra
// beyond the canonical ladder and is never requested by name.
var claudeEfforts = []inference.ReasoningEffort{
	inference.ReasoningLow,
	inference.ReasoningMedium,
	inference.ReasoningHigh,
	inference.ReasoningXHigh,
	"max",
}

// catalog is the built-in model list, aligned with the Claude lineup of
// July 2026. Fable 5 and Mythos 5 keep adaptive thinking always on (the API
// rejects thinking: disabled); the rest of the family can toggle thinking.
var catalog = map[string]catalogEntry{
	"claude-fable-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningAlways),
		efforts:        claudeEfforts,
		maxInputTokens: 1_000_000,
	},
	// claude-mythos-5 shares Fable 5's capabilities and always-on adaptive
	// thinking; it is a limited-release model (Project Glasswing), so
	// deployments must supply access explicitly.
	"claude-mythos-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningAlways),
		efforts:        claudeEfforts,
		maxInputTokens: 1_000_000,
	},
	"claude-opus-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:        claudeEfforts,
		maxInputTokens: 1_000_000,
	},
	"claude-sonnet-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:        claudeEfforts,
		maxInputTokens: 1_000_000,
	},
	"claude-haiku-4-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:        claudeEfforts,
		maxInputTokens: 200_000,
	},
	"claude-haiku-4-5-20251001": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:        claudeEfforts,
		maxInputTokens: 200_000,
	},

	"claude-opus-4-8": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:    claudeEfforts,
		deprecated: true, replacement: "claude-opus-5",
		maxInputTokens: 1_000_000,
	},
	"claude-opus-4-7": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:    claudeEfforts,
		deprecated: true, replacement: "claude-opus-5",
		maxInputTokens: 1_000_000,
	},
	"claude-sonnet-4-6": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:    claudeEfforts,
		deprecated: true, replacement: "claude-sonnet-5",
		maxInputTokens: 1_000_000,
	},
	"claude-sonnet-4-5": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:    claudeEfforts,
		deprecated: true, replacement: "claude-sonnet-5",
		maxInputTokens: 200_000,
	},
	"claude-opus-4-1": {
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(inference.ReasoningToggle),
		efforts:    claudeEfforts,
		deprecated: true, replacement: "claude-opus-5",
		maxInputTokens: 200_000,
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, model := range spec.Models {
		models[model.Name] = catalogEntry{
			capabilities: model.Capabilities,
		}
	}
	for name, entry := range models {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", name, err)
		}
	}
	return models, nil
}
