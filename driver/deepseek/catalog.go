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
// search, and the reasoning control capability. api is a wire-surface fact,
// not a content capability, and stays a separate flag: every catalog model
// is served on the surface Spec.API selects.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// api is the provider-level generate surface selected by Spec.API.
	api apiMode
	// limits carries the model's context/output windows in tokens. Nil
	// leaves are undeclared. Both V4 models carry the 1M context and 384K
	// maximum output published on
	// https://api-docs.deepseek.com/quick_start/pricing.
	limits inference.ModelLimits
	// requestMetadataEnvelope is the provider-level lowering policy for
	// canonical GenerateRequest.RequestMetadata ("" disables forwarding).
	requestMetadataEnvelope string
}

// deepseekEffortMap is the canonical-to-wire map the V4 family declares.
// DeepSeek's public thinking-mode ladder is low/high/max: low stays low,
// medium folds onto high, and the canonical top level is sent as the
// provider's max wire level (DeepSeek's own xhigh request token folds to
// high, so max is how this capability exposes the top tier). Minimal is
// not a DeepSeek wire level and folds onto low. Both deepseek-v4-flash
// and deepseek-v4-pro publish the same mapping.
var deepseekEffortMap = map[inference.ReasoningEffort]string{
	inference.ReasoningMinimal: string(inference.ReasoningLow),
	inference.ReasoningLow:     string(inference.ReasoningLow),
	inference.ReasoningMedium:  string(inference.ReasoningHigh),
	inference.ReasoningHigh:    string(inference.ReasoningHigh),
	inference.ReasoningXHigh:   "max",
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
	return e.limits.Validate()
}

// generateChatCapabilities is the capability declaration for the DeepSeek
// text compiler family: text/data/tool parts in, text out. The base family
// consumes no image/audio/video input; the vision model extends it with
// image input.
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
//   - https://api-docs.deepseek.com/news/news260821
//
// The legacy `deepseek-chat` / `deepseek-reasoner` aliases retired on
// 2026-07-24 and are deliberately absent. Both V4 models are hybrid
// thinking models (thinking enabled by default) with a 1M token context.
// The Responses API serves both V4 models; deepseek-v4-pro support
// landed after the initial flash-only launch. The experimental vision
// model matches V4-Flash's text/agent capabilities, adds image input
// (JPEG/PNG/GIF/WebP via URL or base64), and supports the same Responses
// surface and hosted web search. It has no video or audio input.
var catalog = map[string]catalogEntry{
	"deepseek-v4-flash": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle).
			WithReasoningEffortMap(deepseekEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(384_000),
	},
	"deepseek-v4-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle).
			WithReasoningEffortMap(deepseekEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(384_000),
	},
	"deepseek-v4-flash-vision-exp": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(inference.ReasoningToggle).
			WithReasoningEffortMap(deepseekEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(384_000),
	},
}

// mergedCatalog overlays the built-in catalog with the spec's model
// declarations: a spec entry with a catalog name is a leaf-level patch over
// the built-in entry (capability leaves and numeric limits are inherited
// unless declared explicitly), and unknown names extend the catalog. Models
// stay fail closed — the factory only exposes what the merged catalog
// declares. Every generate model is served on whichever surface Spec.API
// selects.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, declared := range spec.Models {
		entry := catalogEntry{
			kind: modelKind(declared.Kind),
		}
		if entry.kind == "" {
			if existing, exists := models[declared.Name]; exists {
				entry.kind = existing.kind
			} else {
				entry.kind = kindGenerate
			}
		}
		if existing, exists := models[declared.Name]; exists {
			entry.capabilities = declared.Capabilities.Apply(
				existing.capabilities,
			)
			entry.limits = existing.limits.Clone()
		} else {
			entry.capabilities = declared.Capabilities.Apply(
				inference.ModelCapabilities{},
			)
		}
		if declared.Limits.MaxInputTokens != nil {
			value := *declared.Limits.MaxInputTokens
			entry.limits.MaxInputTokens = &value
		}
		if declared.Limits.MaxOutputTokens != nil {
			value := *declared.Limits.MaxOutputTokens
			entry.limits.MaxOutputTokens = &value
		}
		models[declared.Name] = entry
	}
	envelope := spec.requestMetadataEnvelope()
	for name, entry := range models {
		entry.requestMetadataEnvelope = envelope
		models[name] = entry
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
