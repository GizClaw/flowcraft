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
// search, the reasoning control capability, and custom embed output
// dimensions. Reasoning off is not a separate flag: on the Responses
// surface OpenAI expresses it as reasoning.effort="none", so a model
// published as ReasoningToggle has an off route by construction (see
// mergedCatalog for the per-surface resolution).
type catalogEntry struct {
	kind         modelKind
	api          apiMode
	capabilities inference.ModelCapabilities
	// chatStreamIncludeUsage controls whether chat-mode streams ask for
	// the usage chunk via stream_options.include_usage. Nil keeps the
	// driver default (true). It only affects generate entries served by
	// the chat surface; responses-mode streams always include usage.
	chatStreamIncludeUsage *bool
	// chatStreamIncludeObfuscation controls whether chat-mode streams send
	// stream_options.include_obfuscation. Nil keeps the OpenAI default
	// (true); false disables stream obfuscation. It only affects generate
	// entries served by the chat surface.
	chatStreamIncludeObfuscation *bool
	deprecated                   bool
	replacement                  string
	// limits carries the model's context/output windows in tokens. Nil
	// leaves are undeclared. Generate values mirror the context window and
	// maximum output on https://developers.openai.com/api/docs/models;
	// embedding values mirror the per-request input limit.
	limits inference.ModelLimits
	// requestMetadataEnvelope is the provider-level lowering policy for
	// canonical GenerateRequest.RequestMetadata ("" disables forwarding).
	requestMetadataEnvelope string
	// store asks the provider to retain responses server-side. The driver
	// default is false: FlowCraft replays context itself.
	store bool
	// omitReasoningPayload suppresses include: [reasoning.encrypted_content].
	// The zero value keeps the OpenAI shape — reasoning traces round-trip
	// into later context — so an entry built outside mergedCatalog still
	// describes the surface it belongs to.
	omitReasoningPayload bool
	// reasoningChannel selects the reasoning round-trip shape: summary text
	// plus an opaque payload, or plain reasoning text.
	reasoningChannel reasoningChannel
	// reasoningSummary asks the provider for readable reasoning summaries;
	// empty leaves the API default (no summary text).
	reasoningSummary reasoningSummaryPolicy
	// truncation selects the provider's context-overflow policy.
	truncation truncationMode
	// azureDeployment marks endpoints whose data-plane routes carry the
	// deployment name in the path.
	azureDeployment bool
}

// validate enforces the family contract: the compiler bound by kind can only
// serve the output modalities it produces, so kind and capabilities cannot
// drift.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	// Publishing an input kind the wire cannot carry would promise a
	// capability the compiler has to reject at request time.
	for _, kind := range e.capabilities.Inputs {
		switch kind {
		case message.PartAudio:
			return fmt.Errorf(
				"the OpenAI wire has no audio input; drop it from capabilities.inputs",
			)
		case message.PartVideo:
			return fmt.Errorf(
				"the OpenAI wire has no video input; drop it from capabilities.inputs",
			)
		case message.PartFile:
			return fmt.Errorf(
				"the OpenAI wire has no file input; drop it from capabilities.inputs",
			)
		}
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
	return e.limits.Validate()
}

// includeChatStreamUsage resolves the chat streaming usage policy to the
// concrete wire decision.
func (e catalogEntry) includeChatStreamUsage() bool {
	return e.chatStreamIncludeUsage == nil || *e.chatStreamIncludeUsage
}

// chatStreamObfuscation returns the explicit stream obfuscation policy, or
// nil when the OpenAI default should apply.
func (e catalogEntry) chatStreamObfuscation() *bool {
	return e.chatStreamIncludeObfuscation
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
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_050_000).
			WithMaxOutputTokens(128_000),
	},
	"gpt-5.6-terra": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_050_000).
			WithMaxOutputTokens(128_000),
	},
	"gpt-5.6-luna": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningToggle).WithReasoningEffortMap(openaiEffortMap),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_050_000).
			WithMaxOutputTokens(128_000),
	},
	// Generate — previous generations, superseded but available.
	"gpt-5.5": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-sol",
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_050_000).
			WithMaxOutputTokens(128_000),
	},
	"gpt-5.4": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-sol",
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_050_000).
			WithMaxOutputTokens(128_000),
	},
	"gpt-5.4-mini": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-terra",
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(400_000).
			WithMaxOutputTokens(128_000),
	},
	"gpt-5.4-nano": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch().WithReasoning(inference.ReasoningAlways).WithReasoningEffortMap(openaiEffortMap),
		deprecated:   true, replacement: "gpt-5.6-luna",
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(400_000).
			WithMaxOutputTokens(128_000),
	},
	// Generate — GPT-4.1 line: vision without the reasoning control.
	"gpt-4.1": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_047_576).
			WithMaxOutputTokens(32_768),
	},
	"gpt-4.1-mini": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage).WithHostedWebSearch(),
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_047_576).
			WithMaxOutputTokens(32_768),
	},
	// gpt-4.1-nano has no hosted web_search tool.
	"gpt-4.1-nano": {
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithInputs(message.PartImage),
		deprecated:   true, replacement: "gpt-5.6-luna",
		limits: inference.ModelLimits{}.
			WithMaxInputTokens(1_047_576).
			WithMaxOutputTokens(32_768),
	},

	// Embed.
	"text-embedding-3-small": {
		kind:         kindEmbed,
		capabilities: inference.ModelCapabilities{}.WithCustomEmbedDimensions(),
		limits:       inference.ModelLimits{}.WithMaxInputTokens(8_192),
	},
	"text-embedding-3-large": {
		kind:         kindEmbed,
		capabilities: inference.ModelCapabilities{}.WithCustomEmbedDimensions(),
		limits:       inference.ModelLimits{}.WithMaxInputTokens(8_192),
	},
	"text-embedding-ada-002": {
		kind:       kindEmbed,
		deprecated: true, replacement: "text-embedding-3-small",
		limits: inference.ModelLimits{}.WithMaxInputTokens(8_192),
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

// mergedCatalog overlays Spec.Models onto the built-in catalog and resolves
// every entry to the provider instance's surface. A declaration that names a
// built-in model under the same kind is a leaf-level patch: capability
// leaves it names replace the built-in's, everything else (capability
// leaves, numeric limits) is inherited unless declared explicitly.
// Reasoning kind "toggle" is then resolved per surface: OpenAI expresses
// reasoning off as reasoning.effort="none" on Responses only, so a model
// published as "toggle" there is a deployment assertion that the endpoint
// honors that route, while the chat surface cannot express off at all and
// those entries publish "always" instead of promising a switch the
// compiler would reject.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	// A declared catalog starts empty: deployment names must never inherit
	// OpenAI facts just because they collide with a built-in slug.
	if spec.catalogMode() == catalogBuiltinDeclared {
		maps.Copy(models, catalog)
	}
	for _, model := range spec.Models {
		kind := modelKind(model.Kind)
		builtin, exists := models[model.Name]
		entry := catalogEntry{kind: kind}
		if exists && builtin.kind == kind {
			// Same-kind redeclarations inherit the built-in capabilities as
			// the patch base plus the limits below.
			entry.capabilities = model.Capabilities.Apply(builtin.capabilities)
			entry.limits = builtin.limits.Clone()
		} else {
			entry.capabilities = model.Capabilities.Apply(inference.ModelCapabilities{})
		}
		if model.Limits.MaxInputTokens != nil {
			value := *model.Limits.MaxInputTokens
			entry.limits.MaxInputTokens = &value
		}
		if model.Limits.MaxOutputTokens != nil {
			value := *model.Limits.MaxOutputTokens
			entry.limits.MaxOutputTokens = &value
		}
		models[model.Name] = entry
	}
	envelope := spec.requestMetadataEnvelope()
	chatStreamIncludeUsage := spec.chatStreamIncludeUsage()
	chatStreamIncludeObfuscation := spec.chatStreamIncludeObfuscation()
	api := spec.apiMode()
	channel := spec.reasoningChannel()
	reasoningSummary := spec.reasoningSummaryPolicy()
	truncation := spec.truncation()
	omitReasoningPayload := !spec.includeReasoningPayload()
	store := spec.store()
	azureDeployment := spec.routing() == routingAzureDeployment
	for name, entry := range models {
		entry.api = api
		entry.requestMetadataEnvelope = envelope
		entry.chatStreamIncludeUsage = chatStreamIncludeUsage
		entry.chatStreamIncludeObfuscation = chatStreamIncludeObfuscation
		entry.store = store
		entry.reasoningChannel = channel
		entry.reasoningSummary = reasoningSummary
		entry.truncation = truncation
		entry.omitReasoningPayload = omitReasoningPayload
		entry.azureDeployment = azureDeployment
		if entry.kind == kindGenerate &&
			entry.capabilities.Reasoning.Kind == inference.ReasoningToggle &&
			entry.api == apiChat {
			// Chat Completions has no way to express reasoning off for any
			// model, so the truthful per-surface capability is always-on.
			// The text reasoning channel keeps its declaration instead:
			// whether that surface can disable reasoning is an endpoint
			// fact (DeepSeek honors reasoning.effort="none", Kimi does not),
			// so the catalog states it rather than the driver guessing.
			entry.capabilities.Reasoning.Kind = inference.ReasoningAlways
		}
		models[name] = entry
	}
	for name, entry := range models {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
	}
	return models, nil
}

// descriptorFor lowers one catalog entry into its public discovery
// descriptor under id. buildProvider and Catalog share this lowering so
// offline catalog views cannot drift from deployed provider models.
func descriptorFor(id inference.ModelID, entry catalogEntry) inference.ModelDescriptor {
	descriptor := inference.ModelDescriptor{
		ID:           id,
		Capabilities: entry.capabilities,
	}
	if entry.deprecated {
		descriptor.Lifecycle.Status = inference.ModelStatusDeprecated
		if entry.replacement != "" {
			replacement := inference.ModelID{
				Provider: id.Provider,
				Name:     entry.replacement,
			}
			descriptor.Lifecycle.Replacement = &replacement
		}
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
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	slices.Sort(names)
	descriptors := make([]inference.ModelDescriptor, 0, len(catalog))
	for _, name := range names {
		descriptor := descriptorFor(
			inference.ModelID{Provider: provider, Name: name},
			catalog[name],
		)
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}
