package openai

import (
	"encoding/json"
	"sort"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// This file owns the driver's dialect vocabulary: the normalized enums the
// compiler reads, and the one value that carries every provider-wide wire
// decision a model's compiler needs. Spec holds what the deployment writes;
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

// storePolicy is the wire decision for the provider's server-side retention
// field. It is an explicit three-way value rather than a *bool so that the
// zero dialect keeps the pre-existing behavior (send false): omitting the
// field has to be asked for, never inherited from an unbuilt entry.
type storePolicy uint8

// bodyField is one unmodeled body assignment a deployment configured through
// wire.extra_body, pre-sorted at dialect build time so every request the
// deployment serves writes the fields in the same order.
type bodyField struct {
	path  string
	value json.RawMessage
}

// sortedBodyFields orders one body-field map by path. It is derived once per
// provider instance (the dialect is shared by every model's compiler), so the
// request path only copies the slice.
func sortedBodyFields(entries map[string]json.RawMessage) []bodyField {
	if len(entries) == 0 {
		return nil
	}
	fields := make([]bodyField, 0, len(entries))
	for path, value := range entries {
		fields = append(fields, bodyField{path: path, value: value})
	}
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].path < fields[j].path
	})
	return fields
}

const (
	// storeDisabled sends store: false, the driver default.
	storeDisabled storePolicy = iota
	// storeEnabled sends store: true.
	storeEnabled
	// storeOmitted leaves the field off the request entirely.
	storeOmitted
)

// dialect is one provider instance's wire policy: what the deployment decided
// that is not about a single model. It is derived from the Spec once per
// provider and shared by every model's compiler, and every optional is
// already resolved — the compiler reads decisions, never config defaults.
//
// Contracts with more than one leaf are grouped by the layer they come from,
// so a read says where the deployment wrote it: the surface being spoken, the
// reasoning round trip, the body fields the driver does not model, and the
// chat-only streaming policy. A leaf that carries one decision stays at the
// top level.
type dialect struct {
	surface    surfaceDialect
	reasoning  reasoningDialect
	body       bodyDialect
	chat       chatDialect
	store      storePolicy
	truncation truncationMode
	video      bool
}

// surfaceDialect is where requests go: which API family.
type surfaceDialect struct {
	api apiMode
}

// reasoningDialect is the reasoning contract this endpoint speaks: the
// round-trip shape, the readable summary policy, whether the encrypted payload
// rides along, and the verification scope its traces belong to.
type reasoningDialect struct {
	channel       reasoningChannel
	summary       reasoningSummaryPolicy
	payload       bool
	scopeDeclared string
}

// bodyDialect is what the driver forwards without modelling: the deployment's
// own body fields (already ordered) and the envelope that carries canonical
// request metadata.
type bodyDialect struct {
	extra         []bodyField
	metadataField string
}

// chatDialect is the Chat Completions streaming policy. usage is resolved
// (the OpenAI default is to send it); obfuscation stays a tri-state because
// the wire has three states: send true, send false, or leave the field off.
type chatDialect struct {
	usage       bool
	obfuscation *bool
}

// dialect derives the provider-wide wire policy from the spec.
func (s Spec) dialect() dialect {
	return dialect{
		surface: surfaceDialect{
			api: s.apiMode(),
		},
		reasoning: reasoningDialect{
			channel:       s.reasoningChannel(),
			summary:       s.reasoningSummaryPolicy(),
			payload:       s.includeReasoningPayload(),
			scopeDeclared: s.Wire.ReasoningScope,
		},
		body: bodyDialect{
			extra:         sortedBodyFields(s.Wire.ExtraBody),
			metadataField: s.requestMetadataEnvelope(),
		},
		chat: chatDialect{
			usage:       s.chatStreamUsage(),
			obfuscation: s.chatStreamIncludeObfuscation(),
		},
		store:      s.store(),
		truncation: s.truncation(),
		video:      s.Wire.VideoInput,
	}
}

// narrow folds the entry onto the surface the dialect actually speaks: a
// toggle declaration cannot survive on a surface whose implementation has no
// "off" channel, so the published capability becomes always-on. Endpoints
// whose protocol does have an off token keep their declaration — that is an
// endpoint fact the deployment states, not something the driver guesses from
// the reasoning channel.
func (d dialect) narrowModel(kind modelKind, declared ModelSpec) ModelSpec {
	if kind == kindGenerate &&
		declared.Capabilities.Reasoning.Kind == model.ReasoningToggle &&
		d.surface.api == apiChat {
		declared.Capabilities.Reasoning.Kind = model.ReasoningAlways
	}
	return declared
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
// is neither needed nor desirable. "omit" means the field is left off the
// wire entirely, for endpoints that reject request fields their schema does
// not know.
func (s Spec) store() storePolicy {
	if s.Wire.Store == nil {
		return storeDisabled
	}
	if !s.Wire.Store.Send {
		return storeOmitted
	}
	if s.Wire.Store.Value {
		return storeEnabled
	}
	return storeDisabled
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
// resolved: the OpenAI default is to include the usage chunk.
func (s Spec) chatStreamUsage() bool {
	if s.Wire.ChatStreamOptions == nil || s.Wire.ChatStreamOptions.IncludeUsage == nil {
		return true
	}
	return *s.Wire.ChatStreamOptions.IncludeUsage
}

// chatStreamIncludeObfuscation returns the provider policy for chat stream
// obfuscation, or nil when the OpenAI default should apply.
func (s Spec) chatStreamIncludeObfuscation() *bool {
	if s.Wire.ChatStreamOptions == nil {
		return nil
	}
	return s.Wire.ChatStreamOptions.IncludeObfuscation
}
