package bytedance

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// ---------------------------------------------------------------------------
// Wire model — provider-owned, concrete, canonical-free.
//
// The compiler lowers a canonical GenerateRequest into generateWire: plain Go
// values that preserve the request's part order, bytes, and intent verbatim.
// Only the transport converts the wire into ark protobuf messages, so the
// compiled form stays inspectable and free of protobuf oneof interfaces.
// ---------------------------------------------------------------------------

type generateWire struct {
	model        string
	instructions string
	items        []wireItem
	textFormat   *wireTextFormat
	maxTokens    *int64
	temperature  *float64
	topP         *float64
	reasoning    *wireReasoning
	tools        []wireTool
	toolChoice   *wireToolChoice
	stream       bool
	// Extension settings (GenerateOptions).
	thinking           *bool // explicit switch; nil follows reasoning
	serviceTier        string
	caching            *wireCaching
	store              *bool
	previousResponseID string
	parallelToolCalls  *bool
	maxToolCalls       *int64
	webSearch          *wireWebSearch
}

type wireCaching struct {
	enabled bool
	prefix  bool
}

type wireWebSearch struct {
	limit      *int64
	maxKeyword *int32
	sources    []string
	city       string
	country    string
	region     string
	timezone   string
}

type wireReasoning struct {
	effort string // low | medium | high
}

type wireItemKind string

const (
	wireItemMessage    wireItemKind = "message"
	wireItemToolCall   wireItemKind = "tool_call"
	wireItemToolResult wireItemKind = "tool_result"
)

type wireItem struct {
	kind    wireItemKind
	role    string // message: user | assistant
	content []wireContent
	callID  string // tool_call / tool_result
	name    string // tool_call
	args    []byte // tool_call: JSON object
	output  string // tool_result
}

type wireContentKind string

const (
	wireContentText  wireContentKind = "text"
	wireContentImage wireContentKind = "image"
	wireContentVideo wireContentKind = "video"
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

// ---------------------------------------------------------------------------
// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id           string
	reasonings   []rawReasoning // reasoning items in output order
	texts        []string       // output_text items in order
	toolCalls    []rawToolCall
	finish       inference.FinishReason
	usage        rawUsage
	failedReason string // non-empty when the provider reported failure
}

// rawReasoning lowers one reasoning item: joined summary text and the item
// id. ark signs nothing, so the trace cannot round-trip; it is display-only.
type rawReasoning struct {
	id   string
	text string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	inputTokens     int64
	outputTokens    int64
	totalTokens     int64
	cachedTokens    int64
	reasoningTokens int64
}

// streamRaw is one provider stream event. The streaming transport assigns
// canonical part indices (it is the stateful stage) so the decoder function
// stays pure and concurrency-safe.
type streamRaw struct {
	kind   streamRawKind
	part   int    // canonical part index (text / tool / reasoning kinds)
	text   string // text / summary delta
	id     string // terminal reasoning item id
	tool   streamRawTool
	usage  *rawUsage
	finish inference.FinishReason
}

type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawToolFragment
	streamRawReasoning
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
	order     []inference.FieldID // rejection order, deterministic
}

func newLedger(
	operation inference.Operation,
	active []inference.FieldID,
) *ledger {
	return &ledger{
		operation: operation,
		active:    append([]inference.FieldID(nil), active...),
		rejected:  make(map[inference.FieldID]string),
		dropped:   make(map[inference.FieldID]string),
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
		fmt.Errorf("bytedance: %s", l.rejected[field]),
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
	endpoint string,
	entry catalogEntry,
) inference.GenerateCompiler[generateWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[generateWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := generateWire{
			model:  endpoint,
			stream: shape == inference.GenerateExecutionStream,
		}

		// Context messages → items. System text folds into the native
		// instructions field; non-text system parts have no native home.
		var system []string
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				for _, part := range turn.Content.Parts {
					text, ok := part.(message.TextPart)
					if !ok {
						ledger.reject(
							contextPartFields[part.Kind()],
							"system messages carry text only on the Responses API",
						)
						continue
					}
					system = append(system, text.Text)
				}
			case message.RoleTool:
				compileToolResults(&wire, turn.Content.Parts, contextPartFields, ledger)
			default: // user / assistant
				compileMessage(&wire, string(turn.Role), turn.Content.Parts, entry, contextPartFields, ledger)
			}
		}
		wire.instructions = strings.Join(system, "\n\n")

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(&wire, request.Input.Content.Parts, inputPartFields, ledger)
		default:
			compileMessage(&wire, "user", request.Input.Content.Parts, entry, inputPartFields, ledger)
		}

		compileIntent(&wire, request.Input.Content.Intent, entry, ledger)

		// Provider options: GenerateOptions fields lower onto the wire one by
		// one; extensions for other operations are rejected wholesale.
		options, other := operationExtensions[GenerateOptions](request.Extensions)
		rejectOtherExtensions("generate", other, ledger)
		compileGenerateOptions(&wire, options, request, ledger)

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
	_ inference.GenerateRequest,
	ledger *ledger,
) {
	if options.ServiceTier != "" {
		wire.serviceTier = options.ServiceTier
	}
	if options.Caching != nil {
		wire.caching = &wireCaching{
			enabled: options.Caching.Enabled,
			prefix:  options.Caching.Prefix,
		}
	}
	if options.Store != nil {
		wire.store = options.Store
	}
	if options.PreviousResponseID != "" {
		wire.previousResponseID = options.PreviousResponseID
	}
	if options.ParallelToolCalls != nil {
		wire.parallelToolCalls = options.ParallelToolCalls
	}
	if options.MaxToolCalls != nil {
		wire.maxToolCalls = options.MaxToolCalls
	}
	if options.WebSearch != nil {
		search := options.WebSearch
		wire.webSearch = &wireWebSearch{
			limit:      search.Limit,
			maxKeyword: search.MaxKeyword,
			sources:    append([]string(nil), search.Sources...),
			city:       search.UserLocation.City,
			country:    search.UserLocation.Country,
			region:     search.UserLocation.Region,
			timezone:   search.UserLocation.Timezone,
		}
	}
}

// compileMessage appends one user/assistant message's parts to the wire. The
// ark item model separates function calls from messages, so a message with
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
			if !entry.vision {
				ledger.reject(fields[message.PartImage], "model does not accept image input")
				continue
			}
			content = append(content, wireContent{
				kind: wireContentImage,
				uri:  sourceURI(value.Source),
			})
		case message.VideoPart:
			if !entry.video {
				ledger.reject(fields[message.PartVideo], "model does not accept video input")
				continue
			}
			content = append(content, wireContent{
				kind: wireContentVideo,
				uri:  videoSourceURI(value.Source),
			})
		case message.AudioPart:
			ledger.reject(fields[message.PartAudio], "audio input is not supported by generate models")
		case message.FilePart:
			ledger.reject(fields[message.PartFile], "file references are not supported")
		case message.DataPart:
			ledger.reject(fields[message.PartData], "opaque data parts have no native representation")
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
				output: value.Result.Content,
			})
		case message.ReasoningPart:
			flush()
			field := fields[message.PartReasoning]
			if role != "assistant" {
				ledger.reject(field, "reasoning parts belong to assistant context")
				continue
			}
			// ark emits reasoning traces but signs nothing and consumes no
			// reasoning input: the trace cannot round-trip, so it drops
			// with the reason on the ledger rather than vanishing.
			ledger.drop(field, "ark does not consume reasoning input")
		}
	}
	flush()
}

// compileToolResults appends tool-role content. ark carries no error flag on
// tool outputs; the result content is preserved verbatim.
func compileToolResults(
	wire *generateWire,
	parts []message.Part,
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
			output: result.Result.Content,
		})
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
			"text models do not generate images; route a seedream model",
		)
	}
	if intent.Audio != nil {
		ledger.reject(
			inference.FieldGenerateIntentAudio,
			"text models do not synthesize speech; route a seed-tts model",
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
		if !entry.reasoning {
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking control",
			)
		} else {
			wire.thinking = text.ReasoningEnabled
		}
	}
	if text.ReasoningEffort != "" {
		if !entry.reasoning {
			ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no thinking control",
			)
		} else {
			wire.reasoning = &wireReasoning{effort: string(text.ReasoningEffort)}
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

func videoSourceURI(source media.VideoSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func bytesClone(raw []byte) []byte {
	return append([]byte(nil), raw...)
}
