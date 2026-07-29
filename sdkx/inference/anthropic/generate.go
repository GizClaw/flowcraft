package anthropic

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
// Only the transport converts the wire into anthropic-sdk-go param types, so
// the compiled form stays inspectable and free of SDK union wrappers.
// ---------------------------------------------------------------------------

// DefaultMaxTokens pins the required max_tokens parameter when the request
// leaves MaxOutputTokens unset: the Messages API rejects requests without
// it, and inventing a per-model figure would be a silent behavior choice.
const DefaultMaxTokens = 8192

type generateWire struct {
	model       string
	stream      bool
	system      []string
	messages    []wireMessage
	maxTokens   int64
	temperature *float64
	topP        *float64
	effort      string // low | medium | high; empty means unset
	format      *wireFormat
	tools       []wireTool
	toolChoice  *wireToolChoice
}

type wireMessage struct {
	role   string // user | assistant
	blocks []wireBlock
}

type wireBlockKind string

const (
	wireBlockText             wireBlockKind = "text"
	wireBlockImage            wireBlockKind = "image"
	wireBlockToolUse          wireBlockKind = "tool_use"
	wireBlockToolResult       wireBlockKind = "tool_result"
	wireBlockThinking         wireBlockKind = "thinking"
	wireBlockRedactedThinking wireBlockKind = "redacted_thinking"
)

type wireBlock struct {
	kind wireBlockKind
	text string // text / thinking
	// image carries an absolute URL or base64 bytes with a media type.
	imageURL  string
	imageData []byte
	imageType string
	// tool_use / tool_result.
	callID string
	name   string // tool_use
	args   []byte // tool_use: JSON object
	output string // tool_result
	// signature carries the thinking block's verification signature; for
	// redacted_thinking it carries the opaque redacted data.
	signature string
}

// thinking reports whether the block belongs at the head of an assistant
// message: the Messages API requires thinking and redacted blocks to precede
// all other blocks in their turn.
func (b wireBlock) thinking() bool {
	return b.kind == wireBlockThinking || b.kind == wireBlockRedactedThinking
}

type wireFormat struct {
	schema []byte // JSON schema object
}

type wireTool struct {
	name        string
	description string
	schema      []byte
}

type wireToolChoice struct {
	mode string // auto | none | any | tool
	name string
}

// ---------------------------------------------------------------------------
// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id         string
	reasonings []rawReasoning // thinking / redacted blocks in order
	texts      []string       // text blocks in order
	toolCalls  []rawToolCall
	finish     inference.FinishReason
	usage      rawUsage
}

// rawReasoning lowers one thinking block. Text is empty for redacted blocks,
// whose opaque data rides the signature slot.
type rawReasoning struct {
	text      string
	signature string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
}

// streamRaw is one provider stream event. The streaming transport assigns
// canonical part indices (it is the stateful stage) so the decoder function
// stays pure and concurrency-safe.
type streamRaw struct {
	kind      streamRawKind
	part      int    // canonical part index (text / tool / reasoning kinds)
	text      string // text / thinking delta
	signature string // terminal reasoning signature (redacted data included)
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
		fmt.Errorf("anthropic: %s", l.rejected[field]),
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
			model:     model,
			stream:    shape == inference.GenerateExecutionStream,
			maxTokens: DefaultMaxTokens,
		}

		// Context messages. The system channel joins the request's system
		// blocks; everything else becomes user/assistant turns.
		for _, message := range request.Context {
			switch message.Role {
			case inference.RoleSystem:
				compileSystem(&wire, message.Content.Parts, contextPartFields, ledger)
			case inference.RoleTool:
				compileToolResults(&wire, message.Content.Parts, contextPartFields, ledger)
			default: // user / assistant
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
			ledger.reject(field, "anthropic generate supports no extensions")
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[generateWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

// appendBlock adds one block to the wire, merging into the previous message
// when roles collide: the Messages API rejects consecutive same-role turns,
// and tool results always ride a user-role message. Thinking blocks hoist
// ahead of non-thinking blocks inside their turn, which the API requires.
func (w *generateWire) appendBlock(role string, block wireBlock) {
	if block.kind == wireBlockToolResult {
		role = "user"
	}
	if last := len(w.messages) - 1; last >= 0 && w.messages[last].role == role {
		message := &w.messages[last]
		if block.thinking() {
			message.blocks = hoistThinking(message.blocks, block)
			return
		}
		message.blocks = append(message.blocks, block)
		return
	}
	w.messages = append(w.messages, wireMessage{
		role:   role,
		blocks: []wireBlock{block},
	})
}

// hoistThinking inserts a thinking block after the message's existing
// thinking prefix, preserving the relative order of thinking blocks while
// keeping them ahead of text and tool blocks.
func hoistThinking(blocks []wireBlock, block wireBlock) []wireBlock {
	at := 0
	for at < len(blocks) && blocks[at].thinking() {
		at++
	}
	out := make([]wireBlock, 0, len(blocks)+1)
	out = append(out, blocks[:at]...)
	out = append(out, block)
	return append(out, blocks[at:]...)
}

// compileSystem lowers system and developer messages into the request's
// system blocks. Only text survives: the system channel carries no images
// or tool blocks.
func compileSystem(
	wire *generateWire,
	parts []inference.Part,
	fields map[inference.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		if value, ok := part.(inference.TextPart); ok {
			wire.system = append(wire.system, value.Text)
			continue
		}
		ledger.reject(
			fields[part.Kind()],
			"system blocks carry text only",
		)
	}
}

// compileMessage appends one user or assistant message's parts to the wire.
// Tool calls stay assistant-side blocks; tool results move to the user role.
func compileMessage(
	wire *generateWire,
	role string,
	parts []inference.Part,
	entry catalogEntry,
	fields map[inference.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		switch value := part.(type) {
		case inference.TextPart:
			wire.appendBlock(role, wireBlock{kind: wireBlockText, text: value.Text})
		case inference.ImagePart:
			if !entry.vision {
				ledger.reject(fields[inference.PartImage], "model does not accept image input")
				continue
			}
			wire.appendBlock(role, imageBlock(value.Source))
		case inference.AudioPart:
			ledger.reject(fields[inference.PartAudio], "audio input is not supported by claude models")
		case inference.VideoPart:
			ledger.reject(fields[inference.PartVideo], "video input is not supported by claude models")
		case inference.FilePart:
			ledger.reject(fields[inference.PartFile], "file references are not supported")
		case inference.DataPart:
			ledger.reject(fields[inference.PartData], "opaque data parts have no native representation")
		case inference.ToolCallPart:
			wire.appendBlock("assistant", wireBlock{
				kind:   wireBlockToolUse,
				callID: value.Call.ID,
				name:   value.Call.Name,
				args:   bytesClone(value.Call.Arguments),
			})
		case inference.ToolResultPart:
			wire.appendBlock("user", wireBlock{
				kind:   wireBlockToolResult,
				callID: value.Result.CallID,
				output: value.Result.Content,
			})
		case inference.ReasoningPart:
			compileReasoning(wire, role, value, fields, ledger)
		}
	}
}

// compileReasoning lowers an assistant reasoning trace into thinking blocks.
// The Messages API verifies thinking blocks against their signature on
// round-trip, so unsigned reasoning cannot be forwarded honestly and is
// dropped with the reason on the ledger; redacted traces (empty text)
// round-trip through the opaque data slot.
func compileReasoning(
	wire *generateWire,
	role string,
	part inference.ReasoningPart,
	fields map[inference.PartKind]inference.FieldID,
	ledger *ledger,
) {
	field := fields[inference.PartReasoning]
	if role != "assistant" {
		ledger.reject(field, "reasoning parts belong to assistant context")
		return
	}
	if part.Signature == "" {
		ledger.drop(field, "unsigned reasoning cannot round-trip through the messages api")
		return
	}
	if part.Text == "" {
		wire.appendBlock("assistant", wireBlock{
			kind:      wireBlockRedactedThinking,
			signature: part.Signature,
		})
		return
	}
	wire.appendBlock("assistant", wireBlock{
		kind:      wireBlockThinking,
		text:      part.Text,
		signature: part.Signature,
	})
}

// compileToolResults appends tool-role content as user-side tool_result
// blocks.
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
		wire.appendBlock("user", wireBlock{
			kind:   wireBlockToolResult,
			callID: result.Result.CallID,
			output: result.Result.Content,
		})
	}
}

// imageBlock lowers an image source: URLs pass through, inline bytes carry
// their raw data plus media type for base64 encoding at the transport.
func imageBlock(source media.ImageSource) wireBlock {
	if source.Kind() == media.SourceURL {
		return wireBlock{kind: wireBlockImage, imageURL: source.URL()}
	}
	return wireBlock{
		kind:      wireBlockImage,
		imageData: bytesClone(source.Bytes()),
		imageType: source.MediaType(),
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
				ledger.reject(
					inference.FieldGenerateIntentTextResponseKind,
					"claude has no json_object mode; supply a schema for structured output",
				)
			case inference.ResponseJSONSchema:
				wire.format = &wireFormat{schema: bytesClone(format.Schema)}
			}
		}
		if text.MaxOutputTokens != nil {
			wire.maxTokens = int64(*text.MaxOutputTokens)
		}
	}
	if intent.Image != nil {
		ledger.reject(
			inference.FieldGenerateIntentImage,
			"claude models do not generate images",
		)
	}
	if intent.Audio != nil {
		ledger.reject(
			inference.FieldGenerateIntentAudio,
			"claude models do not synthesize speech",
		)
	}
	if intent.Video != nil {
		ledger.reject(
			inference.FieldGenerateIntentVideo,
			"claude models do not generate video",
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
				wire.toolChoice = &wireToolChoice{mode: "any"}
			case inference.ToolChoiceNamed:
				wire.toolChoice = &wireToolChoice{mode: "tool", name: choice.Name}
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
			wire.effort = string(reasoning.Effort)
		}
	}
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

// base64Encode renders inline image bytes for the transport.
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
