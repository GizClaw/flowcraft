package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ---------------------------------------------------------------------------
// Surface sink — the seam between the shared lowering and one OpenAI surface.
//
// The compiler below owns every decision: which parts the model can carry,
// what a refusal costs, which knob a surface can express. A sink is how one
// surface spells those decisions, and it writes them straight into that
// surface's SDK params — the compiled form is the SDK request itself, so the
// driver owns no neutral request model in between.
// ---------------------------------------------------------------------------

// contentPart is one carried content piece: text (structured data lowered to
// text included), an image, or a video the surface addresses by URI. It exists
// only as a sink argument; nothing accumulates it into a request model.
type contentPart struct {
	kind contentPartKind
	text string
	uri  string
}

type contentPartKind int

const (
	contentText contentPartKind = iota
	contentImage
	contentVideo
)

// generateSink is implemented by each OpenAI surface — Responses and Chat
// Completions. Every method is called at most once per decision the compiler
// made, and only for a decision the surface can honor: a knob a surface cannot
// express is rejected on the ledger before the sink is asked for it.
type generateSink interface {
	// --- conversation items -------------------------------------------------

	// message appends one turn's carried content.
	message(role string, content []contentPart)
	// toolCall appends an assistant function call. Chat Completions attaches
	// it to the assistant turn it follows; the Responses API emits a
	// function_call item of its own.
	toolCall(callID, name string, args []byte)
	// toolResult appends one tool result's carried content.
	toolResult(callID string, content []contentPart)
	// reasoning appends one assistant reasoning trace. plain selects the
	// plain-text channel (the trace verbatim) over the summary channel
	// (summary text plus the encrypted payload that verifies it).
	reasoning(trace message.ReasoningPart, plain bool)

	// --- output intent ------------------------------------------------------

	setTextFormat(format *inference.ResponseFormat)
	setMaxOutputTokens(tokens int64)
	setTemperature(value float64)
	setTopP(value float64)
	addTool(definition message.ToolDefinition)
	setToolChoice(choice inference.ToolChoice)
	setReasoningEffort(effort string)
	setVerbosity(level string)

	// --- provider options ---------------------------------------------------

	setServiceTier(tier string)
	setParallelToolCalls(value bool)
	setMaxToolCalls(calls int)
	setSafetyIdentifier(identifier string)
	setPromptCacheKey(key string)
	// setRequestMetadata forwards the caller's metadata bag. envelope names
	// the body field: "metadata" is the typed OpenAI object, any other name
	// is a field the SDK leaves untyped.
	setRequestMetadata(envelope string, metadata map[string]string)
	// setJSONField places one caller-supplied body field the driver does not
	// model. path is sjson notation; value is the raw JSON to place there.
	setJSONField(path string, value json.RawMessage)
	addHostedWebSearch(search *GenerateWebSearch, required bool)
}

// ---------------------------------------------------------------------------
// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id             string
	reasonings     []rawReasoning // reasoning items in output order
	texts          []string       // output_text items in order
	toolCalls      []rawToolCall
	webSearchCalls []inference.WebSearchCall
	citations      []inference.Citation
	finish         inference.FinishReason
	usage          rawUsage
}

// rawReasoning lowers one reasoning item: id for round-trip addressing,
// joined summary text, and the encrypted payload in the signature slot.
type rawReasoning struct {
	id        string
	text      string
	signature string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	inputTokens              int64
	outputTokens             int64
	totalTokens              int64
	cachedTokens             int64
	cacheWriteTokens         int64
	reasoningTokens          int64
	acceptedPredictionTokens int64
	rejectedPredictionTokens int64
	inputAudioTokens         int64
	outputAudioTokens        int64
}

// streamRaw is one provider stream event. The streaming transport assigns
// canonical part indices (it is the stateful stage) so the decoder function
// stays pure and concurrency-safe.
type streamRaw struct {
	kind       streamRawKind
	part       int    // canonical part index (text / tool / reasoning kinds)
	text       string // text / summary delta
	signature  string // terminal reasoning encrypted payload
	id         string // terminal reasoning item id
	responseID string // response-level id from the terminal event
	tool       streamRawTool
	usage      *rawUsage
	finish     inference.FinishReason
	// synthesized marks a finish reason fabricated by the adapter when
	// the stream ended without a provider terminal event (e.g. a clean
	// EOF between frames). It rides the finish raw to the decoded event.
	synthesized     bool
	providerOutputs inference.ProviderOutputs
}

type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawToolFragment
	streamRawReasoning
	streamRawProviderOutput
	streamRawFinish
)

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

// partField resolves a part kind to its ledger field through core's table, so
// the driver cannot drift from the field list the runtime activates. A miss
// cannot happen — core pins the table against message.PartKinds — and panics
// rather than returning an empty field, because a decision recorded against ""
// never reaches the report.
func partField(
	lookup func(message.PartKind) (inference.FieldID, bool),
) func(message.PartKind) inference.FieldID {
	return func(kind message.PartKind) inference.FieldID {
		field, ok := lookup(kind)
		if !ok {
			panic("openai: no ledger field for part kind " + string(kind))
		}
		return field
	}
}

var (
	contextPartField = partField(inference.GenerateContextPartField)
	inputPartField   = partField(inference.GenerateInputPartField)
)

// compileGenerate lowers a canonical request into one surface's SDK request.
// It never downgrades silently: parts the model cannot consume natively are
// rejected on the ledger with a precise reason, and the sink only ever sees
// what the ledger already accounted for.
func compileGenerate[W generateSink](
	entry catalogEntry,
	newSink func(inference.GenerateExecutionShape) W,
) inference.GenerateCompiler[W] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[W], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		sink := newSink(shape)

		if len(request.RequestMetadata) > 0 {
			envelope := entry.dialect.requestMetadataEnvelope
			if envelope == "" {
				ledger.Drop(
					inference.FieldGenerateRequestMetadata,
					"openai request_metadata forwarding is disabled (set spec.request_metadata.envelope)",
				)
			} else {
				sink.setRequestMetadata(envelope, maps.Clone(request.RequestMetadata))
			}
		}

		// Context messages keep their order: system, user, and assistant turns
		// become messages, tool turns become tool results. A turn that
		// interleaves text with tool parts keeps that order through the sink.
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleTool:
				compileToolResults(sink, turn.Content.Parts, entry, contextPartField, ledger)
			default: // system / user / assistant
				compileMessage(sink, string(turn.Role), turn.Content.Parts, entry, contextPartField, ledger)
			}
		}

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(sink, request.Input.Content.Parts, entry, inputPartField, ledger)
		default:
			compileMessage(sink, "user", request.Input.Content.Parts, entry, inputPartField, ledger)
		}

		compileIntent(sink, request.Input.Content.Intent, entry, ledger)

		// Provider options: GenerateOptions fields lower onto the request one
		// by one; extensions for other operations are rejected wholesale.
		options, other := inference.ExtensionFor[GenerateOptions](request.Extensions)
		ledger.RejectExtensions("generate", other)
		compileGenerateOptions(sink, options, entry, ledger)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[W]{Report: report}, ledger.Err()
		}
		return inference.Compiled[W]{Wire: sink, Report: report}, nil
	}
}

// compileGenerateOptions lowers GenerateOptions onto the request.
func compileGenerateOptions(
	sink generateSink,
	options GenerateOptions,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	compileGenerateTuning(sink, options, entry, ledger)
	if options.WebSearch == nil {
		return
	}
	if entry.dialect.api == apiChat {
		ledger.Reject(
			inference.ExtensionField("web_search").Qualify(options),
			"chat completions does not support hosted web search",
		)
		return
	}
	if !entry.capabilities.HostedWebSearch {
		ledger.Reject(
			inference.ExtensionField("web_search").Qualify(options),
			"model does not support hosted web search",
		)
		return
	}
	search := options.WebSearch
	sink.addHostedWebSearch(search, search.ToolChoice != nil && search.ToolChoice.Required)
}

// compileGenerateTuning lowers the per-request knobs that tune one call:
// service tier, tool-call limits, verbosity, and the safety identifier. Each
// lands only where the surface can carry it; anywhere else it is rejected
// with a qualified field name rather than silently dropped.
func compileGenerateTuning(
	sink generateSink,
	options GenerateOptions,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	if tier := options.ServiceTier; tier != "" {
		field := inference.ExtensionField("service_tier").Qualify(options)
		if !validServiceTier(tier) {
			ledger.Reject(field, "unknown service tier \""+tier+"\"")
		} else {
			sink.setServiceTier(tier)
		}
	}
	if options.ParallelToolCalls != nil {
		sink.setParallelToolCalls(*options.ParallelToolCalls)
	}
	if calls := options.MaxToolCalls; calls != nil {
		field := inference.ExtensionField("max_tool_calls").Qualify(options)
		switch {
		case entry.dialect.api == apiChat:
			ledger.Reject(field, "chat completions has no max tool call budget")
		case *calls <= 0:
			ledger.Reject(field, "max_tool_calls must be positive")
		default:
			sink.setMaxToolCalls(*calls)
		}
	}
	if verbosity := options.Verbosity; verbosity != "" {
		field := inference.ExtensionField("verbosity").Qualify(options)
		if !validVerbosity(verbosity) {
			ledger.Reject(field, "unknown verbosity \""+verbosity+"\"")
		} else {
			sink.setVerbosity(verbosity)
		}
	}
	if identifier := options.SafetyIdentifier; identifier != "" {
		sink.setSafetyIdentifier(identifier)
	}
	if key := options.PromptCacheKey; key != "" {
		sink.setPromptCacheKey(key)
	}
	compileGenerateBodyFields(sink, options, entry, ledger)
}

// compileGenerateBodyFields applies the two sources of unmodeled body fields
// in a fixed order: the deployment's wire.extra_body, then the request's
// json_set. Both are written in sorted path order so one request always
// produces the same wire bytes, and a request key therefore wins over the
// deployment default — an identical path replaces it, a nested path merges
// into the object the deployment wrote.
//
// Neither source is guessed: each value was validated where it was declared
// (bounds, well-formed JSON, and no key under a field the compiler owns). The
// deployment's fields are configuration, like `store` or `endpoint.headers`,
// so they carry no report decision; the request's keys are request decisions
// and do, which is what keeps the wallet honest about "this value rode the
// call".
//
// One field name the request-level validation cannot know about is reserved at
// compile time: a deployment that forwards request metadata owns its envelope
// field, and letting json_set write the same name would leave two channels
// fighting over one body field.
func compileGenerateBodyFields(
	sink generateSink,
	options GenerateOptions,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	for _, field := range entry.dialect.extraBody {
		sink.setJSONField(field.path, field.value)
	}
	if len(options.JSONSet) == 0 {
		return
	}
	envelope := entry.dialect.requestMetadataEnvelope
	for _, field := range sortedBodyFields(options.JSONSet) {
		if envelope != "" && jsonSetRoot(field.path) == envelope {
			ledger.Reject(
				inference.ExtensionField(jsonSetFieldName(field.path)).Qualify(options),
				"json_set cannot write the request_metadata envelope field",
			)
			continue
		}
		sink.setJSONField(field.path, field.value)
	}
}

// mediaRoleReason reports why one surface cannot carry media on a message
// role, or "" when it can. Chat Completions lowers every non-user turn to
// text, and a Responses output message carries output_text only; a part on
// one of those turns has no wire form, so the compiler rejects it rather than
// letting the sink drop it after the ledger recorded it as carried.
func mediaRoleReason(api apiMode, role string) string {
	switch {
	case role == string(message.RoleAssistant) && api == apiChat:
		return "chat completions lowers assistant content to text"
	case role == string(message.RoleAssistant):
		return "the responses output message carries output_text only"
	case api == apiChat && role != string(message.RoleUser):
		return "chat completions carries media on user turns only"
	}
	return ""
}

// compileMessage appends one turn's parts. The compiler flushes a run of
// carried content as one message, so a turn that interleaves text with tool
// parts keeps its order: text becomes message content in place, and each tool
// part becomes the item the surface spells it as.
func compileMessage(
	sink generateSink,
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	var content []contentPart
	flush := func() {
		if len(content) == 0 {
			return
		}
		sink.message(role, content)
		content = nil
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			content = append(content, contentPart{kind: contentText, text: value.Text})
		case message.ImagePart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
				ledger.Reject(fields(message.PartImage), "model does not accept image input")
				continue
			}
			// Rejecting on a role the surface lowers to text keeps the ledger
			// honest: appending the part and letting the sink drop it would
			// report a decision the request never carried.
			if reason := mediaRoleReason(entry.dialect.api, role); reason != "" {
				ledger.Reject(fields(message.PartImage), reason)
				continue
			}
			content = append(content, contentPart{
				kind: contentImage,
				uri:  sourceURI(value.Source),
			})
		case message.AudioPart:
			ledger.Reject(fields(message.PartAudio), "audio input is not supported by generate models")
		case message.VideoPart:
			switch {
			case !entry.dialect.videoInput:
				ledger.Reject(
					fields(message.PartVideo),
					"endpoint does not accept video input "+
						"(declare it with spec.wire.video_input on api \"chat\")",
				)
				continue
			case entry.dialect.api != apiChat:
				// Unreachable through Spec validation, which keeps the fact on
				// the chat surface; kept for entries built directly.
				ledger.Reject(
					fields(message.PartVideo),
					"the responses surface has no video input lowering",
				)
				continue
			case !slices.Contains(entry.capabilities.Inputs, message.PartVideo):
				ledger.Reject(fields(message.PartVideo), "model does not accept video input")
				continue
			case value.Source.Kind() == media.SourceStream:
				ledger.Reject(
					fields(message.PartVideo),
					"stream media sources must be materialized before generate",
				)
				continue
			}
			if reason := mediaRoleReason(entry.dialect.api, role); reason != "" {
				ledger.Reject(fields(message.PartVideo), reason)
				continue
			}
			content = append(content, contentPart{
				kind: contentVideo,
				uri:  sourceURI(value.Source),
			})
		case message.FilePart:
			ledger.Reject(fields(message.PartFile), "file references are not supported")
		case message.DataPart:
			content = append(content, contentPart{
				kind: contentText,
				text: "\n" + string(value.Value) + "\n",
			})
		case message.ToolCallPart:
			flush()
			sink.toolCall(value.Call.ID, value.Call.Name, value.Call.Arguments)
		case message.ToolResultPart:
			flush()
			sink.toolResult(
				value.Result.CallID,
				compileToolResultContent(
					value.Result.Content,
					entry,
					fields(message.PartToolResult),
					ledger,
				),
			)
		case message.ReasoningPart:
			flush()
			compileReasoning(sink, role, value, entry, fields, ledger)
		}
	}
	flush()
}

// compileReasoning lowers an assistant reasoning trace into the round-trip
// shape the surface speaks. A trace the surface or its channel cannot express
// drops with the reason on the ledger, never silently.
func compileReasoning(
	sink generateSink,
	role string,
	part message.ReasoningPart,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	field := fields(message.PartReasoning)
	if role != string(message.RoleAssistant) {
		ledger.Reject(field, "reasoning parts belong to assistant context")
		return
	}
	if entry.capabilities.Reasoning.Kind == model.ReasoningNone {
		ledger.Drop(field, "model has no reasoning channel")
		return
	}
	if entry.dialect.api == apiChat {
		// Chat Completions has no standardized reasoning round-trip: the
		// trace cannot ride along, so the drop is reported rather than the
		// assistant turn silently losing it.
		ledger.Drop(field, "chat completions does not replay reasoning items")
		return
	}
	if entry.dialect.reasoningChannel == channelText {
		if part.Text == "" {
			ledger.Drop(
				field,
				"plain reasoning channel requires reasoning text to round-trip",
			)
			return
		}
		sink.reasoning(part, true)
		return
	}
	if entry.dialect.omitReasoningPayload {
		ledger.Drop(
			field,
			"endpoint does not return reasoning payloads "+
				"(wire.include_reasoning_payload is false)",
		)
		return
	}
	if part.Signature == "" || part.ID == "" {
		ledger.Drop(
			field,
			"reasoning items require their id and encrypted payload to round-trip",
		)
		return
	}
	sink.reasoning(part, false)
}

// compileToolResults appends tool-role content. The surface carries text,
// image, and file output, so a multimodal result reaches the model intact when
// the model declares the matching input kind; anything else is replaced by a
// placeholder naming what could not ride along and reported on the ledger.
func compileToolResults(
	sink generateSink,
	parts []message.Part,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			ledger.Reject(
				fields(part.Kind()),
				"tool-role content carries tool results only",
			)
			continue
		}
		sink.toolResult(
			result.Result.CallID,
			compileToolResultContent(
				result.Result.Content,
				entry,
				fields(message.PartToolResult),
				ledger,
			),
		)
	}
}

// compileToolResultContent lowers one tool result's content parts. Text and
// structured data always ride; an image needs a surface that carries tool
// images at all (Chat Completions tool messages are text-only), a model that
// declares image input, and a source the transport can address. A part that
// cannot ride is replaced in place by a text placeholder, so the model sees
// where something was missing instead of only that something was, and the
// loss lands on the ledger.
func compileToolResultContent(
	content message.Content,
	entry catalogEntry,
	field inference.FieldID,
	ledger *inference.Ledger,
) []contentPart {
	vision := slices.Contains(entry.capabilities.Inputs, message.PartImage)
	chatSurface := entry.dialect.api == apiChat
	out := make([]contentPart, 0, len(content.Parts))
	omitted := make([]string, 0, len(content.Parts))
	notes := make([]inference.ComponentNote, 0, len(content.Parts))
	degraded := false
	for index, part := range content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			out = append(out, contentPart{kind: contentText, text: value.Text})
			notes = append(notes, carriedComponent(message.PartText, index))
		case message.DataPart:
			out = append(out, contentPart{
				kind: contentText,
				text: "\n" + string(value.Value) + "\n",
			})
			notes = append(notes, carriedComponent(message.PartData, index))
		case message.ImagePart:
			var reason string
			switch {
			case chatSurface:
				reason = "image (chat tool messages carry text only)"
			case !vision:
				reason = "image (model does not accept image input)"
			case value.Source.Kind() == media.SourceStream:
				reason = "image (stream source was not materialized)"
			}
			if reason != "" {
				omitted = append(omitted, reason)
				notes = append(notes,
					droppedComponent(message.PartImage, index, reason))
				degraded = true
				out = append(out, toolResultPlaceholder(reason))
				continue
			}
			out = append(out, contentPart{
				kind: contentImage,
				uri:  sourceURI(value.Source),
			})
			notes = append(notes, carriedComponent(message.PartImage, index))
		default:
			reason := string(part.Kind())
			omitted = append(omitted, reason)
			notes = append(notes,
				droppedComponent(part.Kind(), index, reason))
			degraded = true
			out = append(out, toolResultPlaceholder(reason))
		}
	}
	// Component notes are only worth carrying when something was degraded:
	// a fully native result needs no audit trail.
	if degraded {
		ledger.DropComponents(
			field,
			notes,
			"tool output omitted "+strings.Join(omitted, ", "),
		)
	}
	return out
}

// carriedComponent notes a tool result component that reached the request.
func carriedComponent(kind message.PartKind, index int) inference.ComponentNote {
	return inference.ComponentNote{
		Kind:        kind,
		Disposition: inference.Native,
		Index:       index,
	}
}

// droppedComponent notes a tool result component the request could not carry.
func droppedComponent(
	kind message.PartKind,
	index int,
	reason string,
) inference.ComponentNote {
	return inference.ComponentNote{
		Kind:        kind,
		Disposition: inference.Dropped,
		Index:       index,
		Reason:      reason,
	}
}

// toolResultPlaceholder names one dropped part in a tool result. It keeps the
// part's slot in the content list so the model sees where something was
// missing, not just that something was.
func toolResultPlaceholder(reason string) contentPart {
	return contentPart{
		kind: contentText,
		text: "[omitted tool output: " + reason + "]",
	}
}

func compileIntent(
	sink generateSink,
	intent inference.Intent,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	if text := intent.Text; text != nil {
		if format := text.Response; format != nil {
			switch format.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject, inference.ResponseJSONSchema:
				sink.setTextFormat(format)
			}
		}
		if text.MaxOutputTokens != nil {
			sink.setMaxOutputTokens(int64(*text.MaxOutputTokens))
		}
	}
	if intent.Image != nil {
		ledger.Reject(
			inference.FieldGenerateIntentImage,
			"text models do not generate images; route a gpt-image model",
		)
	}
	if intent.Audio != nil {
		ledger.Reject(
			inference.FieldGenerateIntentAudio,
			"text models do not synthesize speech; route a tts model",
		)
	}
	if intent.Video != nil {
		ledger.Reject(
			inference.FieldGenerateIntentVideo,
			"openai has no video generation surface",
		)
	}
	text := intent.Text
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		sink.addTool(definition)
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceAuto, inference.ToolChoiceNone,
			inference.ToolChoiceRequired, inference.ToolChoiceNamed:
			sink.setToolChoice(*choice)
		}
	}
	if text.Temperature != nil {
		sink.setTemperature(*text.Temperature)
	}
	if text.TopP != nil {
		sink.setTopP(*text.TopP)
	}
	if text.ReasoningEnabled != nil {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no reasoning to switch",
			)
		case entry.capabilities.Reasoning.Kind == model.ReasoningAlways &&
			!*text.ReasoningEnabled:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"openai reasoning models cannot disable reasoning",
			)
		case entry.capabilities.Reasoning.Kind == model.ReasoningToggle &&
			!*text.ReasoningEnabled:
			// Toggle is only published where the surface can express off:
			// Responses lowers it to reasoning.effort "none", while chat
			// entries are lowered to always at merge time. The chat guard
			// stays as compiler-level defense for direct entry misuse.
			if entry.dialect.api != apiChat {
				sink.setReasoningEffort("none")
				break
			}
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"reasoning cannot be disabled through this provider",
			)
		}
		// enabled == true is a no-op: reasoning models reason by default.
	}
	if text.ReasoningEffort != "" {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no reasoning effort control",
			)
		case len(entry.capabilities.Reasoning.EffortMap) == 0:
			// Spec-declared reasoning models without an explicit map keep
			// the legacy pass-through behavior: OpenAI's reasoning.effort
			// accepts the canonical effort tokens verbatim.
			sink.setReasoningEffort(string(text.ReasoningEffort))
		default:
			mode, _ := entry.capabilities.Reasoning.ResolveEffort(
				text.ReasoningEffort,
			)
			sink.setReasoningEffort(mode)
			if mode != string(text.ReasoningEffort) {
				ledger.Drop(
					inference.FieldGenerateIntentReasoningEffort,
					fmt.Sprintf(
						"model maps reasoning effort %q to %q",
						text.ReasoningEffort,
						mode,
					),
				)
			}
		}
	}
}

// rejectTextControls rejects the text-only intent controls (tools, sampling,
// reasoning) for a non-text operation, one decision per active field so the
// report stays field-precise.
func rejectTextControls(
	text *inference.TextIntent,
	ledger *inference.Ledger,
	toolsReason, samplingReason, reasoningReason string,
) {
	if len(text.Tools) > 0 {
		ledger.Reject(inference.FieldGenerateIntentTools, toolsReason)
	}
	if text.ToolChoice != nil {
		ledger.Reject(inference.FieldGenerateIntentToolChoice, toolsReason)
	}
	if text.Temperature != nil {
		ledger.Reject(inference.FieldGenerateIntentTemperature, samplingReason)
	}
	if text.TopP != nil {
		ledger.Reject(inference.FieldGenerateIntentTopP, samplingReason)
	}
	if text.ReasoningEnabled != nil {
		ledger.Reject(inference.FieldGenerateIntentReasoningEnabled, reasoningReason)
	}
	if text.ReasoningEffort != "" {
		ledger.Reject(inference.FieldGenerateIntentReasoningEffort, reasoningReason)
	}
}

// sourceURI renders an image source as the single URI string the API accepts:
// absolute URLs pass through, inline bytes become a data: URI.
// mediaSource is the part of a media source every surface addresses the same
// way: a URL when the source is linked, an inline data URI when it is not.
type mediaSource interface {
	Kind() media.SourceKind
	URL() string
	MediaType() string
	Bytes() []byte
}

func sourceURI[Source mediaSource](source Source) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

// schemaMap lowers a canonical JSON schema into the map shape the SDK's param
// types require; an empty or malformed schema becomes an open object schema.
func schemaMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{"type": "object"}
	}
	return decoded
}
