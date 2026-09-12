package openai

import "github.com/GizClaw/flowcraft/core/inference/model"

// This file owns the driver's dialect vocabulary: the normalized enums the
// compiler reads, and the one value that carries every provider-wide wire
// decision a catalog entry needs. Spec holds what the deployment writes;
// dialect holds what the driver concluded from it.

// apiMode selects the generate wire surface for one provider instance.
type apiMode string

const (
	apiResponses apiMode = "responses"
	apiChat      apiMode = "chat"
)

// reasoningChannel selects the reasoning round-trip shape.
type reasoningChannel string

const (
	// channelSummary carries summary text plus an opaque encrypted payload.
	channelSummary reasoningChannel = "summary"
	// channelText carries plain reasoning text merged into the assistant
	// message, with no encrypted payload.
	channelText reasoningChannel = "text"
)

// reasoningSummaryPolicy selects the readable reasoning summary a provider
// emits; an empty value asks for nothing, matching the API default.
type reasoningSummaryPolicy string

const (
	summaryAuto     reasoningSummaryPolicy = "auto"
	summaryConcise  reasoningSummaryPolicy = "concise"
	summaryDetailed reasoningSummaryPolicy = "detailed"
)

// truncationMode selects the provider's context-overflow policy.
type truncationMode string

const (
	truncationAuto     truncationMode = "auto"
	truncationDisabled truncationMode = "disabled"
)

// dialect is one provider instance's wire policy. Every catalog entry carries
// the same value, stamped once by mergedCatalog: it is deployment
// configuration, not a model fact, and the compiler reads it here instead of
// reaching back into the Spec.
type dialect struct {
	api                     apiMode
	store                   bool
	omitReasoningPayload    bool
	reasoningChannel        reasoningChannel
	reasoningSummary        reasoningSummaryPolicy
	truncation              truncationMode
	azureDeployment         bool
	requestMetadataEnvelope string
	// chatStreamIncludeUsage / Obfuscation carry the explicit chat streaming
	// policy; nil keeps the OpenAI default.
	chatStreamIncludeUsage       *bool
	chatStreamIncludeObfuscation *bool
}

// dialect derives the provider-wide wire policy from the spec.
func (s Spec) dialect() dialect {
	return dialect{
		api:                          s.apiMode(),
		store:                        s.store(),
		omitReasoningPayload:         !s.includeReasoningPayload(),
		reasoningChannel:             s.reasoningChannel(),
		reasoningSummary:             s.reasoningSummaryPolicy(),
		truncation:                   s.truncation(),
		azureDeployment:              s.routing() == routingAzureDeployment,
		requestMetadataEnvelope:      s.requestMetadataEnvelope(),
		chatStreamIncludeUsage:       s.chatStreamIncludeUsage(),
		chatStreamIncludeObfuscation: s.chatStreamIncludeObfuscation(),
	}
}

// narrow folds the entry onto the surface the dialect actually speaks: a
// toggle declaration cannot survive on a surface whose implementation has no
// "off" channel, so the published capability becomes always-on. Endpoints
// whose protocol does have an off token keep their declaration — that is an
// endpoint fact the catalog states, not something the driver guesses from the
// reasoning channel.
func (d dialect) narrow(entry catalogEntry) catalogEntry {
	if entry.kind == kindGenerate &&
		entry.capabilities.Reasoning.Kind == model.ReasoningToggle &&
		d.api == apiChat {
		entry.capabilities.Reasoning.Kind = model.ReasoningAlways
	}
	return entry
}

// chatStreamUsage resolves the chat streaming usage policy to a wire decision.
func (d dialect) chatStreamUsage() bool {
	return d.chatStreamIncludeUsage == nil || *d.chatStreamIncludeUsage
}

// chatObfuscation returns the explicit stream obfuscation policy, or nil when
// the OpenAI default applies.
func (d dialect) chatObfuscation() *bool {
	return d.chatStreamIncludeObfuscation
}

// apiMode returns the normalized generate API mode.
func (s Spec) apiMode() apiMode {
	if s.API == "" {
		return apiResponses
	}
	return apiMode(s.API)
}

// store reports whether responses are retained server-side. The driver
// default is false: FlowCraft replays context itself, so server-side storage
// is neither needed nor desirable.
func (s Spec) store() bool {
	if s.Wire.Store == nil {
		return false
	}
	return *s.Wire.Store
}

// reasoningChannel returns the normalized reasoning round-trip shape.
func (s Spec) reasoningChannel() reasoningChannel {
	if s.Wire.ReasoningChannel == "" {
		return channelSummary
	}
	return reasoningChannel(s.Wire.ReasoningChannel)
}

// reasoningSummaryPolicy returns the configured reasoning summary policy, or
// "" when the deployment leaves it unset.
func (s Spec) reasoningSummaryPolicy() reasoningSummaryPolicy {
	return reasoningSummaryPolicy(s.Wire.ReasoningSummary)
}

// truncation returns the configured overflow policy, or "" when the
// deployment leaves the provider default in place.
func (s Spec) truncation() truncationMode {
	return truncationMode(s.Wire.Truncation)
}

// includeReasoningPayload reports whether reasoning requests ask for the
// encrypted payload. It follows the reasoning channel unless overridden.
func (s Spec) includeReasoningPayload() bool {
	if s.Wire.IncludeReasoningPayload != nil {
		return *s.Wire.IncludeReasoningPayload
	}
	return s.reasoningChannel() == channelSummary
}

func (s Spec) requestMetadataEnvelope() string {
	if s.RequestMetadata == nil {
		return ""
	}
	return s.RequestMetadata.Envelope
}

// chatStreamIncludeUsage returns the provider policy for chat streaming,
// or nil when the driver default (include usage) should apply.
func (s Spec) chatStreamIncludeUsage() *bool {
	if s.Wire.ChatStreamOptions == nil {
		return nil
	}
	return s.Wire.ChatStreamOptions.IncludeUsage
}

// chatStreamIncludeObfuscation returns the provider policy for chat stream
// obfuscation, or nil when the OpenAI default should apply.
func (s Spec) chatStreamIncludeObfuscation() *bool {
	if s.Wire.ChatStreamOptions == nil {
		return nil
	}
	return s.Wire.ChatStreamOptions.IncludeObfuscation
}
