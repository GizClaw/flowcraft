package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
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
	stream      bool
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
	output  string // tool_result
	// reasoning carries one reasoning item round-trip: the item id, the
	// joined summary text, and the encrypted verification payload.
	reasoningID string
	summary     string
	encrypted   string
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

// ---------------------------------------------------------------------------
// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id         string
	reasonings []rawReasoning // reasoning items in output order
	texts      []string       // output_text items in order
	toolCalls  []rawToolCall
	finish     inference.FinishReason
	usage      rawUsage
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
	kind      streamRawKind
	part      int    // canonical part index (text / tool / reasoning kinds)
	text      string // text / summary delta
	signature string // terminal reasoning encrypted payload
	id        string // terminal reasoning item id
	tool      streamRawTool
	usage     *rawUsage
	finish    inference.FinishReason
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
		fmt.Errorf("openai: %s", l.rejected[field]),
	)
}

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

var contextPartFields = map[inference.PartKind]inference.FieldID{
	inference.PartText:       inference.FieldGenerateContextText,
	inference.PartImage:      inference.FieldGenerateContextImage,
	inference.PartAudio:      inference.FieldGenerateContextAudio,
	inference.PartVideo:      inference.FieldGenerateContextVideo,
	inference.PartFile:       inference.FieldGenerateContextFile,
	inference.PartData:       inference.FieldGenerateContextData,
	inference.PartToolCall:   inference.FieldGenerateContextToolCall,
	inference.PartToolResult: inference.FieldGenerateContextToolResult,
	inference.PartReasoning:  inference.FieldGenerateContextReasoning,
}

var inputPartFields = map[inference.PartKind]inference.FieldID{
	inference.PartText:       inference.FieldGenerateInputText,
	inference.PartImage:      inference.FieldGenerateInputImage,
	inference.PartAudio:      inference.FieldGenerateInputAudio,
	inference.PartVideo:      inference.FieldGenerateInputVideo,
	inference.PartFile:       inference.FieldGenerateInputFile,
	inference.PartData:       inference.FieldGenerateInputData,
	inference.PartToolCall:   inference.FieldGenerateInputToolCall,
	inference.PartToolResult: inference.FieldGenerateInputToolResult,
	inference.PartReasoning:  inference.FieldGenerateInputReasoning,
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
			model:  model,
			stream: shape == inference.GenerateExecutionStream,
		}

		// Context messages → items. System stays a native system-role item;
		// the Responses API consumes roles directly.
		for _, message := range request.Context {
			switch message.Role {
			case inference.RoleTool:
				compileToolResults(&wire, message.Content.Parts, contextPartFields, ledger)
			default: // system / user / assistant
				compileMessage(&wire, string(message.Role), message.Content.Parts, entry, contextPartFields, ledger)
			}
		}

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(&wire, request.Input.Content.Parts, inputPartFields, ledger)
		default:
			compileMessage(&wire, "user", request.Input.Content.Parts, entry, inputPartFields, ledger)
		}

		compileIntent(&wire, request.Input.Content.Intent, entry, ledger)

		// No provider extensions exist yet; anything attached is rejected
		// truthfully rather than dropped.
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "openai generate supports no extensions")
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[generateWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

// compileMessage appends one message's parts to the wire. The Responses item
// model separates function calls from messages, so a message with
// interleaved text and tool parts becomes a run of message items plus call
// items in original order.
func compileMessage(
	wire *generateWire,
	role string,
	parts []inference.Part,
	entry catalogEntry,
	fields map[inference.PartKind]inference.FieldID,
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
		case inference.TextPart:
			content = append(content, wireContent{kind: wireContentText, text: value.Text})
		case inference.ImagePart:
			if !entry.vision {
				ledger.reject(fields[inference.PartImage], "model does not accept image input")
				continue
			}
			content = append(content, wireContent{
				kind: wireContentImage,
				uri:  sourceURI(value.Source),
			})
		case inference.AudioPart:
			ledger.reject(fields[inference.PartAudio], "audio input is not supported by generate models")
		case inference.VideoPart:
			ledger.reject(fields[inference.PartVideo], "video input is not supported by generate models")
		case inference.FilePart:
			ledger.reject(fields[inference.PartFile], "file references are not supported")
		case inference.DataPart:
			ledger.reject(fields[inference.PartData], "opaque data parts have no native representation")
		case inference.ToolCallPart:
			flush()
			wire.items = append(wire.items, wireItem{
				kind:   wireItemToolCall,
				callID: value.Call.ID,
				name:   value.Call.Name,
				args:   bytesClone(value.Call.Arguments),
			})
		case inference.ToolResultPart:
			flush()
			wire.items = append(wire.items, wireItem{
				kind:   wireItemToolResult,
				callID: value.Result.CallID,
				output: value.Result.Content,
			})
		case inference.ReasoningPart:
			flush()
			compileReasoning(wire, role, value, entry, fields, ledger)
		}
	}
	flush()
}

// compileReasoning lowers an assistant reasoning trace into a reasoning
// item. The Responses API addresses reasoning items by id and verifies the
// encrypted payload on round-trip, so a trace missing either cannot be
// forwarded honestly; models without a reasoning channel cannot consume
// the item at all. Both cases drop with the reason on the ledger.
func compileReasoning(
	wire *generateWire,
	role string,
	part inference.ReasoningPart,
	entry catalogEntry,
	fields map[inference.PartKind]inference.FieldID,
	ledger *ledger,
) {
	field := fields[inference.PartReasoning]
	if role != "assistant" {
		ledger.reject(field, "reasoning parts belong to assistant context")
		return
	}
	if !entry.reasoning {
		ledger.drop(field, "model has no reasoning channel")
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

// compileToolResults appends tool-role content verbatim; the API carries no
// error flag on tool outputs.
func compileToolResults(
	wire *generateWire,
	parts []inference.Part,
	fields map[inference.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		result, ok := part.(inference.ToolResultPart)
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
	if tools := intent.Tools; tools != nil {
		for _, definition := range tools.Definitions {
			wire.tools = append(wire.tools, wireTool{
				name:        definition.Name,
				description: definition.Description,
				schema:      bytesClone(definition.InputSchema),
			})
		}
		if choice := tools.Choice; choice != nil {
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
	}
	if sampling := intent.Sampling; sampling != nil {
		wire.temperature = sampling.Temperature
		wire.topP = sampling.TopP
	}
	if reasoning := intent.Reasoning; reasoning != nil && reasoning.Effort != "" {
		if !entry.reasoning {
			ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no reasoning effort control",
			)
		} else {
			wire.reasoning = string(reasoning.Effort)
		}
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
