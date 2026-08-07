package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// generateWire is the provider-owned intermediate representation for one
// chat completion request. The transport stage converts it into openai-go
// params; DeepSeek-only knobs (thinking, reasoning_effort) become
// per-request JSON overrides at that point.
type generateWire struct {
	model       string
	messages    []wireMessage
	maxTokens   *int64
	temperature *float64
	topP        *float64
	jsonObject  bool
	tools       []wireTool
	toolChoice  *wireToolChoice
	effort      string // reasoning_effort passthrough; API maps low/medium to high
	thinking    *bool  // explicit thinking switch; nil follows the API default
	stream      bool
}

type wireMessage struct {
	role string // system | user | assistant | tool
	text string
	// reasoning carries the assistant turn's reasoning_content round-trip.
	// DeepSeek requires it verbatim on every assistant turn that performed
	// tool calls while thinking ran, so the compiler attaches it natively
	// there and drops it anywhere else.
	reasoning    string
	hasReasoning bool
	toolCalls    []wireToolCall
	callID       string // tool role: the call this message answers
}

type wireToolCall struct {
	id   string
	name string
	args []byte
}

type wireTool struct {
	name        string
	description string
	parameters  []byte
}

type wireToolChoice struct {
	mode string // auto | none | required | named
	name string
}

// generateRaw is the transport stage's normalized completion: everything
// the decoder needs, nothing it does not.
type generateRaw struct {
	id        string // chat completion id from the wire response
	reasoning string
	text      string
	toolCalls []rawToolCall
	finish    inference.FinishReason
	usage     rawUsage
}

type rawToolCall struct {
	id   string
	name string
	args string
}

type rawUsage struct {
	input     int64
	output    int64
	total     int64
	cached    int64
	reasoning int64
	present   bool
}

// streamRawKind enumerates the events the stateful stream transport hands
// to the pure stream decoder.
type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawReasoning
	streamRawToolFragment
	streamRawFinish
)

type streamRaw struct {
	kind       streamRawKind
	part       int // canonical part index, assigned by the transport
	text       string
	responseID string // chat completion id from the stream chunks
	tool       streamRawTool
	usage      *rawUsage
	finish     inference.FinishReason
}

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

// ledger tracks the compiler's decision for every active request field so
// the report accounts for each one exactly once: rejected (compile fails),
// dropped (intentionally discarded with a reason), or native (consumed).
type ledger struct {
	operation inference.Operation
	active    []inference.FieldID
	rejected  map[inference.FieldID]string
	dropped   map[inference.FieldID]string
	order     []inference.FieldID
}

func newLedger(operation inference.Operation, active []inference.FieldID) *ledger {
	return &ledger{
		operation: operation,
		active:    active,
		rejected:  make(map[inference.FieldID]string),
		dropped:   make(map[inference.FieldID]string),
	}
}

func (l *ledger) reject(field inference.FieldID, reason string) {
	if _, exists := l.rejected[field]; !exists {
		l.order = append(l.order, field)
	}
	l.rejected[field] = reason
}

func (l *ledger) drop(field inference.FieldID, reason string) {
	if _, rejected := l.rejected[field]; rejected {
		return
	}
	if _, exists := l.dropped[field]; !exists {
		l.dropped[field] = reason
	}
}

func (l *ledger) report() inference.CompileReport {
	report := inference.CompileReport{Operation: l.operation}
	for _, field := range l.active {
		decision := inference.Decision{Field: field, Disposition: inference.Native}
		if reason, rejected := l.rejected[field]; rejected {
			decision.Disposition = inference.Rejected
			decision.Reason = reason
		} else if reason, dropped := l.dropped[field]; dropped {
			decision.Disposition = inference.Dropped
			decision.Reason = reason
		}
		report.Decisions = append(report.Decisions, decision)
	}
	return report
}

func (l *ledger) err() error {
	if len(l.order) == 0 {
		return nil
	}
	field := l.order[0]
	reason := l.rejected[field]
	if strings.HasPrefix(string(field), "extension.") {
		return inference.NewError(inference.InvalidExtension, l.operation, field, fmt.Errorf("deepseek: %s", reason))
	}
	return inference.NewError(inference.UnsupportedFeature, l.operation, field, fmt.Errorf("deepseek: %s", reason))
}

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

// compileGenerate compiles a canonical generate request into the chat
// completions wire model. System messages stay messages (the protocol has
// no instructions slot); user and assistant turns map one-to-one; tool
// results become tool-role messages.
func compileGenerate(model string, entry catalogEntry) inference.GenerateCompiler[generateWire] {
	return func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[generateWire], error) {
		ledger := newLedger(inference.OperationGenerate, request.ActiveFieldsFor(shape))
		wire := generateWire{model: model, stream: shape == inference.GenerateExecutionStream}

		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				compileSystemMessage(&wire, turn.Content.Parts, ledger)
			case message.RoleTool:
				compileToolResults(&wire, turn.Content.Parts, contextPartFields, ledger)
			default:
				compileMessage(&wire, string(turn.Role), turn.Content.Parts, entry, contextPartFields, ledger)
			}
		}
		if request.Input.Role == inference.InputRoleTool {
			compileToolResults(&wire, request.Input.Content.Parts, inputPartFields, ledger)
		} else {
			compileMessage(&wire, "user", request.Input.Content.Parts, entry, inputPartFields, ledger)
		}
		compileIntent(&wire, request.Input.Content.Intent, entry, ledger)

		rejectOtherExtensions("generate", request.Extensions, ledger)

		report := ledger.report()
		if err := ledger.err(); err != nil {
			return inference.Compiled[generateWire]{Report: report}, err
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

func compileSystemMessage(wire *generateWire, parts []message.Part, ledger *ledger) {
	var text strings.Builder
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			text.WriteString(value.Text)
		default:
			ledger.reject(contextPartFields[part.Kind()], "system messages carry text only")
		}
	}
	wire.messages = append(wire.messages, wireMessage{role: "system", text: text.String()})
}

// compileMessage appends one chat message per canonical message. Assistant
// turns are where DeepSeek's reasoning round-trip rule applies: a turn that
// performed tool calls must carry its reasoning_content back, a turn that
// did not has no channel for it.
func compileMessage(
	wire *generateWire,
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	var (
		text         strings.Builder
		reasoning    strings.Builder
		sawReasoning bool
		toolCalls    []wireToolCall
	)
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			text.WriteString(value.Text)
		case message.ImagePart:
			ledger.reject(fields[message.PartImage], "deepseek models are text-only")
		case message.VideoPart:
			ledger.reject(fields[message.PartVideo], "deepseek models are text-only")
		case message.AudioPart:
			ledger.reject(fields[message.PartAudio], "deepseek models are text-only")
		case message.FilePart:
			ledger.reject(fields[message.PartFile], "file references are not supported")
		case message.DataPart:
			ledger.reject(fields[message.PartData], "opaque data parts have no native representation")
		case message.ToolCallPart:
			toolCalls = append(toolCalls, wireToolCall{
				id:   value.Call.ID,
				name: value.Call.Name,
				args: bytes.Clone(value.Call.Arguments),
			})
		case message.ToolResultPart:
			ledger.reject(fields[message.PartToolResult], "tool results ride tool-role messages, not user or assistant turns")
		case message.ReasoningPart:
			if role != "assistant" {
				ledger.reject(fields[message.PartReasoning], "reasoning parts belong to assistant context")
				continue
			}
			sawReasoning = true
			reasoning.WriteString(value.Text)
		}
	}

	wireMsg := wireMessage{role: role, text: text.String(), toolCalls: toolCalls}
	if sawReasoning {
		if len(toolCalls) > 0 {
			wireMsg.reasoning = reasoning.String()
			wireMsg.hasReasoning = true
		} else {
			ledger.drop(fields[message.PartReasoning], "deepseek ignores reasoning on turns without tool calls")
		}
	}
	wire.messages = append(wire.messages, wireMsg)
}

func compileToolResults(
	wire *generateWire,
	parts []message.Part,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		switch value := part.(type) {
		case message.ToolResultPart:
			wire.messages = append(wire.messages, wireMessage{
				role:   "tool",
				text:   value.Result.Content,
				callID: value.Result.CallID,
			})
		default:
			ledger.reject(fields[part.Kind()], "tool-role content carries tool results only")
		}
	}
}

func compileIntent(wire *generateWire, intent inference.Intent, entry catalogEntry, ledger *ledger) {
	if text := intent.Text; text != nil {
		if response := text.Response; response != nil {
			switch response.Kind {
			case inference.ResponseText:
			case inference.ResponseJSONObject:
				wire.jsonObject = true
			case inference.ResponseJSONSchema:
				ledger.reject(inference.FieldGenerateIntentTextResponseKind, "deepseek supports json_object responses only, not schema-constrained output")
			}
		}
		if text.MaxOutputTokens != nil {
			wire.maxTokens = new(int64(*text.MaxOutputTokens))
		}
	}
	if intent.Image != nil {
		ledger.reject(inference.FieldGenerateIntentImage, "text models do not generate images")
	}
	if intent.Audio != nil {
		ledger.reject(inference.FieldGenerateIntentAudio, "text models do not generate audio")
	}
	if intent.Video != nil {
		ledger.reject(inference.FieldGenerateIntentVideo, "text models do not generate video")
	}
	text := intent.Text
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		wire.tools = append(wire.tools, wireTool{
			name:        definition.Name,
			description: definition.Description,
			parameters:  bytes.Clone(definition.InputSchema),
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
	wire.temperature = clonePointer(text.Temperature)
	wire.topP = clonePointer(text.TopP)
	if text.ReasoningEnabled != nil {
		if !entry.reasoning {
			ledger.reject(inference.FieldGenerateIntentReasoningEnabled, "model has no thinking control")
		} else {
			wire.thinking = clonePointer(text.ReasoningEnabled)
		}
	}
	if text.ReasoningEffort != "" {
		if !entry.reasoning {
			ledger.reject(inference.FieldGenerateIntentReasoningEffort, "model has no thinking control")
		} else {
			// DeepSeek documents low/medium as aliases for high and
			// xhigh for max: pass the canonical effort through verbatim
			// and let the API normalize it.
			wire.effort = string(text.ReasoningEffort)
		}
	}
}

// jsonMap decodes a raw JSON object for the SDK's function parameters map.
func jsonMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}
