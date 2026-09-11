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
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ---------------------------------------------------------------------------
// Wire model — provider-owned, concrete, canonical-free.
//
// The compiler lowers a canonical GenerateRequest into generateWire: plain Go
// values that preserve the request's part order, bytes, and intent verbatim.
// Only the transport converts the wire into openai-go param types, so the
// compiled form stays inspectable and free of SDK union wrappers.
// ---------------------------------------------------------------------------

type generateWire struct {
	model       string
	items       []wireItem
	textFormat  *wireTextFormat
	maxTokens   *int64
	temperature *float64
	topP        *float64
	reasoning   string // effort; empty means unset
	tools       []wireTool
	toolChoice  *wireToolChoice
	webSearch   *wireWebSearch
	stream      bool
	// store asks the provider to retain the response server-side. Lowered
	// from Spec.Wire.Store; the driver default is false.
	store bool
	// reasoningChannel selects the reasoning round-trip shape this wire
	// speaks: summary text plus an opaque payload, or plain reasoning text.
	reasoningChannel reasoningChannel
	// reasoningSummary asks the provider for readable reasoning summaries.
	reasoningSummary reasoningSummaryPolicy
	// truncation selects the provider's context-overflow policy.
	truncation truncationMode
	// promptCacheKey routes the request to the cache holding its prefix.
	promptCacheKey string
	// serviceTier selects the provider's processing tier for this call.
	serviceTier string
	// parallelToolCalls overrides whether the model may call tools in
	// parallel; nil keeps the provider default.
	parallelToolCalls *bool
	// maxToolCalls caps tool calls inside one response; nil keeps the
	// provider default.
	maxToolCalls *int
	// verbosity tunes output length ("low" | "medium" | "high").
	verbosity string
	// safetyIdentifier is the stable per-user identifier providers use for
	// abuse monitoring.
	safetyIdentifier string
	// chatStreamIncludeUsage asks Chat Completions streams for the usage
	// chunk via stream_options.include_usage. It is a transport policy
	// lowered from the provider spec; Responses mode never consults it.
	chatStreamIncludeUsage bool
	// chatStreamIncludeObfuscation carries the explicit chat stream
	// obfuscation policy. Nil keeps the OpenAI default (obfuscation on);
	// false disables it. Responses mode never consults it.
	chatStreamIncludeObfuscation *bool
	// includeReasoning asks the Responses API to attach the encrypted
	// reasoning payload. Only reasoning models can carry it; Azure rejects
	// the include on plain chat models, so it must follow the capability
	// declaration instead of being unconditional.
	includeReasoning bool

	// requestMetadataEnvelope names the top-level body field that carries
	// canonical request metadata; empty disables forwarding.
	requestMetadataEnvelope string
	// requestMetadata is the opaque metadata bag forwarded verbatim.
	requestMetadata map[string]string
}

type wireItemKind string

const (
	wireItemMessage    wireItemKind = "message"
	wireItemToolCall   wireItemKind = "tool_call"
	wireItemToolResult wireItemKind = "tool_result"
	wireItemReasoning  wireItemKind = "reasoning"
)

type wireItem struct {
	kind    wireItemKind
	role    string // message: system | user | assistant
	content []wireContent
	callID  string // tool_call / tool_result
	name    string // tool_call
	args    []byte // tool_call: JSON object
	// output carries a tool result's lowered content. A single text part
	// keeps the string form every endpoint accepts; richer results ride the
	// content-list form.
	output []wireContent
	// reasoning carries one reasoning item round-trip: the item id, the
	// joined summary text, and the encrypted verification payload.
	reasoningID string
	summary     string
	encrypted   string
	// reasoningText carries the plain reasoning text a text-channel endpoint
	// round-trips in place of summary plus encrypted payload.
	reasoningText string
}

type wireContentKind string

const (
	wireContentText  wireContentKind = "text"
	wireContentImage wireContentKind = "image"
)

type wireContent struct {
	kind wireContentKind
	text string
	// uri carries an absolute URL or a data: URI assembled from inline bytes.
	uri string
}

type wireTextFormat struct {
	kind   string // json_object | json_schema
	name   string
	schema []byte
	strict bool
}

type wireTool struct {
	name        string
	description string
	schema      []byte
}

type wireToolChoice struct {
	mode string // auto | none | required | named
	name string
}

type wireWebSearch struct {
	searchContextSize string
	allowedDomains    []string
	city              string
	country           string
	region            string
	timezone          string
	externalWebAccess *bool
	returnTokenBudget string
	required          bool
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
// Compile ledger — tracks rejected active fields and builds reports.
// ---------------------------------------------------------------------------

type ledger struct {
	operation inference.Operation
	active    []inference.FieldID
	rejected  map[inference.FieldID]string
	dropped   map[inference.FieldID]string
	// components carries per-component detail for fields that aggregate
	// several content parts, populated only when at least one was degraded.
	components map[inference.FieldID][]inference.ComponentNote
	order      []inference.FieldID // rejection order, deterministic
}

func newLedger(
	operation inference.Operation,
	active []inference.FieldID,
) *ledger {
	return &ledger{
		operation:  operation,
		active:     append([]inference.FieldID(nil), active...),
		rejected:   make(map[inference.FieldID]string),
		dropped:    make(map[inference.FieldID]string),
		components: make(map[inference.FieldID][]inference.ComponentNote),
	}
}

func (l *ledger) reject(field inference.FieldID, reason string) {
	if _, exists := l.rejected[field]; !exists {
		l.order = append(l.order, field)
		l.rejected[field] = reason
	}
}

// drop records an intentional discard that keeps the compile successful.
// Rejection wins when both land on one field: a failed compile reports the
// rejection.
func (l *ledger) drop(field inference.FieldID, reason string) {
	if _, rejected := l.rejected[field]; rejected {
		return
	}
	if _, exists := l.dropped[field]; !exists {
		l.dropped[field] = reason
	}
}

// dropComponents records a field-level drop together with the per-component
// notes that explain it. The notes cover every component of the field in
// encounter order, so consumers can tell "the text arrived but the image did
// not" apart from "the whole result was lost".
func (l *ledger) dropComponents(
	field inference.FieldID,
	notes []inference.ComponentNote,
	reason string,
) {
	if len(notes) == 0 {
		l.drop(field, reason)
		return
	}
	l.components[field] = append(l.components[field], notes...)
	l.drop(field, reason)
}

// report renders the compile report: every active field carries exactly one
// disposition — Rejected, then Dropped, otherwise Native.
func (l *ledger) report() inference.CompileReport {
	decisions := make([]inference.Decision, 0, len(l.active))
	for _, field := range l.active {
		if reason, rejected := l.rejected[field]; rejected {
			decisions = append(decisions, inference.Decision{
				Field:       field,
				Disposition: inference.Rejected,
				Reason:      reason,
			})
			continue
		}
		if reason, dropped := l.dropped[field]; dropped {
			decisions = append(decisions, inference.Decision{
				Field:       field,
				Disposition: inference.Dropped,
				Reason:      reason,
				Components:  append([]inference.ComponentNote(nil), l.components[field]...),
			})
			continue
		}
		decisions = append(decisions, inference.Decision{
			Field:       field,
			Disposition: inference.Native,
		})
	}
	return inference.CompileReport{
		Operation: l.operation,
		Decisions: decisions,
	}
}

// err builds the structured compiler rejection. The first rejected field in
// rejection order becomes the error field; extension rejections classify as
// InvalidExtension, everything else as UnsupportedFeature.
func (l *ledger) err() error {
	field := l.order[0]
	kind := inference.UnsupportedFeature
	if strings.HasPrefix(string(field), "extension.") {
		kind = inference.InvalidExtension
	}
	return inference.NewError(
		kind,
		l.operation,
		field,
		fmt.Errorf("openai: %s", l.rejected[field]),
	)
}

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

var contextPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateContextText,
	message.PartImage:      inference.FieldGenerateContextImage,
	message.PartAudio:      inference.FieldGenerateContextAudio,
	message.PartVideo:      inference.FieldGenerateContextVideo,
	message.PartFile:       inference.FieldGenerateContextFile,
	message.PartData:       inference.FieldGenerateContextData,
	message.PartToolCall:   inference.FieldGenerateContextToolCall,
	message.PartToolResult: inference.FieldGenerateContextToolResult,
	message.PartReasoning:  inference.FieldGenerateContextReasoning,
}

var inputPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateInputText,
	message.PartImage:      inference.FieldGenerateInputImage,
	message.PartAudio:      inference.FieldGenerateInputAudio,
	message.PartVideo:      inference.FieldGenerateInputVideo,
	message.PartFile:       inference.FieldGenerateInputFile,
	message.PartData:       inference.FieldGenerateInputData,
	message.PartToolCall:   inference.FieldGenerateInputToolCall,
	message.PartToolResult: inference.FieldGenerateInputToolResult,
	message.PartReasoning:  inference.FieldGenerateInputReasoning,
}

// compileGenerate lowers a canonical request into the provider wire. It never
// downgrades silently: parts the model cannot consume natively are rejected
// in the ledger with a precise reason.
func compileGenerate(
	model string,
	entry catalogEntry,
) inference.GenerateCompiler[generateWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[generateWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := generateWire{
			model:                        model,
			stream:                       shape == inference.GenerateExecutionStream,
			store:                        entry.store,
			reasoningChannel:             entry.reasoningChannel,
			reasoningSummary:             entry.reasoningSummary,
			truncation:                   entry.truncation,
			chatStreamIncludeUsage:       entry.includeChatStreamUsage(),
			chatStreamIncludeObfuscation: entry.chatStreamObfuscation(),
			includeReasoning: entry.capabilities.Reasoning.Kind != inference.ReasoningNone &&
				entry.api != apiChat &&
				!entry.omitReasoningPayload,
		}
		if entry.requestMetadataEnvelope != "" && len(request.RequestMetadata) > 0 {
			wire.requestMetadataEnvelope = entry.requestMetadataEnvelope
			wire.requestMetadata = maps.Clone(request.RequestMetadata)
		} else if len(request.RequestMetadata) > 0 {
			ledger.drop(
				inference.FieldGenerateRequestMetadata,
				"openai request_metadata forwarding is disabled (set spec.request_metadata.envelope)",
			)
		}

		// Context messages → items. System stays a native system-role item;
		// the Responses API consumes roles directly.
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleTool:
				compileToolResults(&wire, turn.Content.Parts, entry, contextPartFields, ledger)
			default: // system / user / assistant
				compileMessage(&wire, string(turn.Role), turn.Content.Parts, entry, contextPartFields, ledger)
			}
		}

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(&wire, request.Input.Content.Parts, entry, inputPartFields, ledger)
		default:
			compileMessage(&wire, "user", request.Input.Content.Parts, entry, inputPartFields, ledger)
		}

		compileIntent(&wire, request.Input.Content.Intent, entry, ledger)

		// Provider options: GenerateOptions fields lower onto the wire one by
		// one; extensions for other operations are rejected wholesale.
		options, other := operationExtensions[GenerateOptions](request.Extensions)
		rejectOtherExtensions("generate", other, ledger)
		compileGenerateOptions(&wire, options, entry, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[generateWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

// compileGenerateOptions lowers GenerateOptions onto the wire.
func compileGenerateOptions(
	wire *generateWire,
	options GenerateOptions,
	entry catalogEntry,
	ledger *ledger,
) {
	compileGenerateTuning(wire, options, entry, ledger)
	if options.WebSearch == nil {
		return
	}
	if entry.api == apiChat {
		ledger.reject(
			inference.ExtensionField("web_search").Qualify(options),
			"chat completions does not support hosted web search",
		)
		return
	}
	if !entry.capabilities.HostedWebSearch {
		ledger.reject(
			inference.ExtensionField("web_search").Qualify(options),
			"model does not support hosted web search",
		)
		return
	}
	search := options.WebSearch
	wire.webSearch = &wireWebSearch{
		searchContextSize: search.SearchContextSize,
		allowedDomains:    append([]string(nil), search.AllowedDomains...),
		city:              search.UserLocation.City,
		country:           search.UserLocation.Country,
		region:            search.UserLocation.Region,
		timezone:          search.UserLocation.Timezone,
		externalWebAccess: clonePointer(search.ExternalWebAccess),
		returnTokenBudget: search.ReturnTokenBudget,
		required:          search.ToolChoice != nil && search.ToolChoice.Required,
	}
}

// compileGenerateTuning lowers the per-request knobs that tune one call:
// service tier, tool-call limits, verbosity, and the safety identifier. Each
// lands only where the surface can carry it; anywhere else it is rejected
// with a qualified field name rather than silently dropped.
func compileGenerateTuning(
	wire *generateWire,
	options GenerateOptions,
	entry catalogEntry,
	ledger *ledger,
) {
	chat := entry.api == apiChat
	if tier := options.ServiceTier; tier != "" {
		field := inference.ExtensionField("service_tier").Qualify(options)
		if !validServiceTier(tier) {
			ledger.reject(field, "unknown service tier \""+tier+"\"")
		} else {
			wire.serviceTier = tier
		}
	}
	if options.ParallelToolCalls != nil {
		wire.parallelToolCalls = clonePointer(options.ParallelToolCalls)
	}
	if calls := options.MaxToolCalls; calls != nil {
		field := inference.ExtensionField("max_tool_calls").Qualify(options)
		switch {
		case chat:
			ledger.reject(field, "chat completions has no max tool call budget")
		case *calls <= 0:
			ledger.reject(field, "max_tool_calls must be positive")
		default:
			wire.maxToolCalls = calls
		}
	}
	if verbosity := options.Verbosity; verbosity != "" {
		field := inference.ExtensionField("verbosity").Qualify(options)
		switch {
		case chat:
			ledger.reject(field, "chat completions has no verbosity control")
		case !validVerbosity(verbosity):
			ledger.reject(field, "unknown verbosity \""+verbosity+"\"")
		default:
			wire.verbosity = verbosity
		}
	}
	if identifier := options.SafetyIdentifier; identifier != "" {
		wire.safetyIdentifier = identifier
	}
	if key := options.PromptCacheKey; key != "" {
		wire.promptCacheKey = key
	}
}

// compileMessage appends one message's parts to the wire. The Responses item
// model separates function calls from messages, so a message with
// interleaved text and tool parts becomes a run of message items plus call
// items in original order.
func compileMessage(
	wire *generateWire,
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	var content []wireContent
	flush := func() {
		if len(content) == 0 {
			return
		}
		wire.items = append(wire.items, wireItem{
			kind:    wireItemMessage,
			role:    role,
			content: content,
		})
		content = nil
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			content = append(content, wireContent{kind: wireContentText, text: value.Text})
		case message.ImagePart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
				ledger.reject(fields[message.PartImage], "model does not accept image input")
				continue
			}
			// Assistant context rides an output-message item, whose content
			// is output_text/refusal only: an assistant image has no wire
			// form, so it fails here rather than reaching the provider as an
			// item the API refuses. Chat completions lowers assistant
			// content to a plain string and keeps its own behavior.
			if role == "assistant" && entry.api != apiChat {
				ledger.reject(
					fields[message.PartImage],
					"assistant context cannot carry image input",
				)
				continue
			}
			content = append(content, wireContent{
				kind: wireContentImage,
				uri:  sourceURI(value.Source),
			})
		case message.AudioPart:
			ledger.reject(fields[message.PartAudio], "audio input is not supported by generate models")
		case message.VideoPart:
			ledger.reject(fields[message.PartVideo], "video input is not supported by generate models")
		case message.FilePart:
			ledger.reject(fields[message.PartFile], "file references are not supported")
		case message.DataPart:
			content = append(content, wireContent{
				kind: wireContentText,
				text: "\n" + string(value.Value) + "\n",
			})
		case message.ToolCallPart:
			flush()
			wire.items = append(wire.items, wireItem{
				kind:   wireItemToolCall,
				callID: value.Call.ID,
				name:   value.Call.Name,
				args:   bytesClone(value.Call.Arguments),
			})
		case message.ToolResultPart:
			flush()
			wire.items = append(wire.items, wireItem{
				kind:   wireItemToolResult,
				callID: value.Result.CallID,
				output: compileToolResultContent(
					value.Result.Content,
					entry,
					fields[message.PartToolResult],
					ledger,
				),
			})
		case message.ReasoningPart:
			flush()
			compileReasoning(wire, role, value, entry, fields, ledger)
		}
	}
	flush()
}

// compileReasoning lowers an assistant reasoning trace into a reasoning
// item. How the trace round-trips follows the wire's reasoning channel:
// a summary channel addresses the item by id and verifies the encrypted
// payload, while a text channel carries the plain trace verbatim. A trace
// the channel cannot express drops with the reason on the ledger, and a
// model without a reasoning channel cannot consume the item at all.
func compileReasoning(
	wire *generateWire,
	role string,
	part message.ReasoningPart,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	field := fields[message.PartReasoning]
	if role != "assistant" {
		ledger.reject(field, "reasoning parts belong to assistant context")
		return
	}
	if entry.capabilities.Reasoning.Kind == inference.ReasoningNone {
		ledger.drop(field, "model has no reasoning channel")
		return
	}
	if entry.reasoningChannel == channelText {
		if part.Text == "" {
			ledger.drop(
				field,
				"plain reasoning channel requires reasoning text to round-trip",
			)
			return
		}
		wire.items = append(wire.items, wireItem{
			kind:          wireItemReasoning,
			reasoningID:   part.ID,
			reasoningText: part.Text,
		})
		return
	}
	if entry.omitReasoningPayload {
		ledger.drop(
			field,
			"endpoint does not return reasoning payloads "+
				"(wire.include_reasoning_payload is false)",
		)
		return
	}
	if part.Signature == "" || part.ID == "" {
		ledger.drop(
			field,
			"reasoning items require their id and encrypted payload to round-trip",
		)
		return
	}
	wire.items = append(wire.items, wireItem{
		kind:        wireItemReasoning,
		reasoningID: part.ID,
		summary:     part.Text,
		encrypted:   part.Signature,
	})
}

// compileToolResults appends tool-role content. The Responses API carries
// text, image, and file output, so a multimodal result reaches the model
// intact when the model declares the matching input kind; anything else is
// replaced by a placeholder naming what could not ride along and reported on
// the ledger.
func compileToolResults(
	wire *generateWire,
	parts []message.Part,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			ledger.reject(
				fields[part.Kind()],
				"tool-role content carries tool results only",
			)
			continue
		}
		wire.items = append(wire.items, wireItem{
			kind:   wireItemToolResult,
			callID: result.Result.CallID,
			output: compileToolResultContent(
				result.Result.Content,
				entry,
				fields[message.PartToolResult],
				ledger,
			),
		})
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
	ledger *ledger,
) []wireContent {
	vision := slices.Contains(entry.capabilities.Inputs, message.PartImage)
	chatSurface := entry.api == apiChat
	out := make([]wireContent, 0, len(content.Parts))
	omitted := make([]string, 0, len(content.Parts))
	notes := make([]inference.ComponentNote, 0, len(content.Parts))
	degraded := false
	for index, part := range content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			out = append(out, wireContent{kind: wireContentText, text: value.Text})
			notes = append(notes, carriedComponent(message.PartText, index))
		case message.DataPart:
			out = append(out, wireContent{
				kind: wireContentText,
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
			out = append(out, wireContent{
				kind: wireContentImage,
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
		ledger.dropComponents(
			field,
			notes,
			"tool output omitted "+strings.Join(omitted, ", "),
		)
	}
	return out
}

// carriedComponent notes a tool result component that reached the wire.
func carriedComponent(kind message.PartKind, index int) inference.ComponentNote {
	return inference.ComponentNote{
		Kind:        kind,
		Disposition: inference.Native,
		Index:       index,
	}
}

// droppedComponent notes a tool result component the wire could not carry.
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
func toolResultPlaceholder(reason string) wireContent {
	return wireContent{
		kind: wireContentText,
		text: "[omitted tool output: " + reason + "]",
	}
}

func compileIntent(
	wire *generateWire,
	intent inference.Intent,
	entry catalogEntry,
	ledger *ledger,
) {
	if text := intent.Text; text != nil {
		if format := text.Response; format != nil {
			switch format.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject:
				wire.textFormat = &wireTextFormat{kind: "json_object"}
			case inference.ResponseJSONSchema:
				wire.textFormat = &wireTextFormat{
					kind:   "json_schema",
					name:   format.Name,
					schema: bytesClone(format.Schema),
					strict: true,
				}
			}
		}
		if text.MaxOutputTokens != nil {
			max := int64(*text.MaxOutputTokens)
			wire.maxTokens = &max
		}
	}
	if intent.Image != nil {
		ledger.reject(
			inference.FieldGenerateIntentImage,
			"text models do not generate images; route a gpt-image model",
		)
	}
	if intent.Audio != nil {
		ledger.reject(
			inference.FieldGenerateIntentAudio,
			"text models do not synthesize speech; route a tts model",
		)
	}
	if intent.Video != nil {
		ledger.reject(
			inference.FieldGenerateIntentVideo,
			"openai has no video generation surface",
		)
	}
	text := intent.Text
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		wire.tools = append(wire.tools, wireTool{
			name:        definition.Name,
			description: definition.Description,
			schema:      bytesClone(definition.InputSchema),
		})
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceAuto:
			wire.toolChoice = &wireToolChoice{mode: "auto"}
		case inference.ToolChoiceNone:
			wire.toolChoice = &wireToolChoice{mode: "none"}
		case inference.ToolChoiceRequired:
			wire.toolChoice = &wireToolChoice{mode: "required"}
		case inference.ToolChoiceNamed:
			wire.toolChoice = &wireToolChoice{mode: "named", name: choice.Name}
		}
	}
	wire.temperature = text.Temperature
	wire.topP = text.TopP
	if text.ReasoningEnabled != nil {
		switch {
		case entry.capabilities.Reasoning.Kind == inference.ReasoningNone:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no reasoning to switch",
			)
		case entry.capabilities.Reasoning.Kind == inference.ReasoningAlways &&
			!*text.ReasoningEnabled:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"openai reasoning models cannot disable reasoning",
			)
		case entry.capabilities.Reasoning.Kind == inference.ReasoningToggle &&
			!*text.ReasoningEnabled:
			// Toggle is only published where the surface can express off:
			// Responses lowers it to reasoning.effort "none", while chat
			// entries are lowered to always at merge time. The chat guard
			// stays as compiler-level defense for direct entry misuse.
			if entry.api != apiChat {
				wire.reasoning = "none"
				break
			}
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"reasoning cannot be disabled through this provider",
			)
		}
		// enabled == true is a no-op: reasoning models reason by default.
	}
	if text.ReasoningEffort != "" {
		switch {
		case entry.capabilities.Reasoning.Kind == inference.ReasoningNone:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no reasoning effort control",
			)
		case len(entry.capabilities.Reasoning.EffortMap) == 0:
			// Spec-declared reasoning models without an explicit map keep
			// the legacy pass-through behavior: OpenAI's reasoning.effort
			// accepts the canonical effort tokens verbatim.
			wire.reasoning = string(text.ReasoningEffort)
		default:
			mode, _ := entry.capabilities.Reasoning.ResolveEffort(
				text.ReasoningEffort,
			)
			wire.reasoning = mode
			if mode != string(text.ReasoningEffort) {
				ledger.drop(
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
	ledger *ledger,
	toolsReason, samplingReason, reasoningReason string,
) {
	if len(text.Tools) > 0 {
		ledger.reject(inference.FieldGenerateIntentTools, toolsReason)
	}
	if text.ToolChoice != nil {
		ledger.reject(inference.FieldGenerateIntentToolChoice, toolsReason)
	}
	if text.Temperature != nil {
		ledger.reject(inference.FieldGenerateIntentTemperature, samplingReason)
	}
	if text.TopP != nil {
		ledger.reject(inference.FieldGenerateIntentTopP, samplingReason)
	}
	if text.ReasoningEnabled != nil {
		ledger.reject(inference.FieldGenerateIntentReasoningEnabled, reasoningReason)
	}
	if text.ReasoningEffort != "" {
		ledger.reject(inference.FieldGenerateIntentReasoningEffort, reasoningReason)
	}
}

// sourceURI renders an image source as the single URI string the API accepts:
// absolute URLs pass through, inline bytes become a data: URI.
func sourceURI(source media.ImageSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func bytesClone(raw []byte) []byte {
	return append([]byte(nil), raw...)
}

// schemaMap lowers a canonical JSON schema into the map shape the SDK's
// param types require; an empty schema becomes an open object schema.
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
