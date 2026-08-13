package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// generateWire is the provider-owned intermediate representation for one
// chat completion request, fully concrete per the runtime's wire
// contract. The transport stage renders the JSON body from it; Kimi-only
// knobs (thinking, reasoning_effort, video_url, prompt_cache_key) are
// first-class fields here.
type generateWire struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`

	MaxTokens        *int64   `json:"max_completion_tokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`

	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
	Tools          []wireTool          `json:"tools,omitempty"`

	// Kimi-owned reasoning dialects.
	Effort       string      `json:"reasoning_effort,omitempty"` // k3: low | high | max
	Thinking     *thinkingOn `json:"thinking,omitempty"`         // K2.x: type (+ keep on k2.6)
	PreserveKeep bool        `json:"-"`                          // k2.6: render thinking.keep="all"

	PromptCacheKey string `json:"prompt_cache_key,omitempty"`

	// toolChoiceRaw renders tool_choice polymorphically (string mode or
	// named-function object); ToolChoice stays out of the struct JSON so
	// the two never disagree.
	toolChoiceRaw json.RawMessage
}

type thinkingOn struct {
	Type string `json:"type"`           // enabled | disabled
	Keep string `json:"keep,omitempty"` // all
}

func (w generateWire) MarshalJSON() ([]byte, error) {
	type alias generateWire
	raw, err := json.Marshal(alias(w))
	if err != nil {
		return nil, err
	}
	if len(w.toolChoiceRaw) == 0 {
		return raw, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["tool_choice"] = w.toolChoiceRaw
	return json.Marshal(body)
}

// wireMessage is one chat message. Content renders as a plain string when
// the turn is text-only, or as an ordered part array when images/videos
// ride along; reasoning_content and tool_calls are OpenAI-extra fields
// Kimi honors.
type wireMessage struct {
	Role       string            `json:"role"`
	Text       string            `json:"-"` // joined text when Parts is empty
	Parts      []wireContentPart `json:"-"` // ordered multimodal content
	Reasoning  string            `json:"reasoning_content,omitempty"`
	ToolCalls  []wireToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// wireContentPart is one element of the multimodal content array, in the
// canonical part order.
type wireContentPart struct {
	Type     string        `json:"type"` // text | image_url | video_url
	Text     string        `json:"text,omitempty"`
	ImageURL *wireMediaURL `json:"image_url,omitempty"`
	VideoURL *wireMediaURL `json:"video_url,omitempty"`
}

type wireMediaURL struct {
	URL string `json:"url"`
}

func (m wireMessage) MarshalJSON() ([]byte, error) {
	type alias wireMessage
	raw, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	var content json.RawMessage
	if len(m.Parts) > 0 {
		content, err = json.Marshal(m.Parts)
	} else {
		content, err = json.Marshal(m.Text)
	}
	if err != nil {
		return nil, err
	}
	body["content"] = content
	return json.Marshal(body)
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireResponseFormat struct {
	Type       string            `json:"type"` // text | json_object | json_schema
	JSONSchema *wireJSONSchemaFn `json:"json_schema,omitempty"`
}

type wireJSONSchemaFn struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
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
		return inference.NewError(inference.InvalidExtension, l.operation, field, fmt.Errorf("kimi: %s", reason))
	}
	return inference.NewError(inference.UnsupportedFeature, l.operation, field, fmt.Errorf("kimi: %s", reason))
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
// completions wire model. System messages stay messages; user and
// assistant turns map one-to-one (with multimodal part arrays where the
// model accepts them); tool results become tool-role messages.
func compileGenerate(model string, entry catalogEntry) inference.GenerateCompiler[generateWire] {
	return func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[generateWire], error) {
		ledger := newLedger(inference.OperationGenerate, request.ActiveFieldsFor(shape))
		wire := generateWire{Model: model, Stream: shape == inference.GenerateExecutionStream}

		options, other := operationExtensions[GenerateOptions](request.Extensions)
		rejectOtherExtensions("generate", other, ledger)

		sawHistoryReasoning := false
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				wire.Messages = append(wire.Messages, compileSystemMessage(turn.Content.Parts, ledger))
			case message.RoleTool:
				compileToolResults(&wire, turn.Content.Parts, contextPartFields, ledger)
			default:
				msg := compileTurnMessage(string(turn.Role), turn.Content.Parts, entry, contextPartFields, ledger)
				if msg.Role == "assistant" && msg.Reasoning != "" {
					sawHistoryReasoning = true
				}
				wire.Messages = append(wire.Messages, msg)
			}
		}
		if request.Input.Role == inference.InputRoleTool {
			compileToolResults(&wire, request.Input.Content.Parts, inputPartFields, ledger)
		} else {
			wire.Messages = append(wire.Messages, compileTurnMessage("user", request.Input.Content.Parts, entry, inputPartFields, ledger))
		}
		compileIntent(&wire, request.Input.Content.Intent, entry, ledger)
		compileExtensions(&wire, options, entry, sawHistoryReasoning, ledger)

		report := ledger.report()
		if err := ledger.err(); err != nil {
			return inference.Compiled[generateWire]{Report: report}, err
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

func compileSystemMessage(parts []message.Part, ledger *ledger) wireMessage {
	var text strings.Builder
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			text.WriteString(value.Text)
		case message.DataPart:
			text.WriteString("\n" + string(value.Value) + "\n")
		default:
			ledger.reject(contextPartFields[part.Kind()], "system messages carry text only")
		}
	}
	return wireMessage{Role: "system", Text: text.String()}
}

// compileTurnMessage appends one user or assistant message. Multimodal
// parts render as content part arrays on models that accept them;
// assistant reasoning traces round-trip as reasoning_content where the
// model re-ingests them.
func compileTurnMessage(
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) wireMessage {
	wireMsg := wireMessage{Role: role}
	var (
		text      strings.Builder
		mediaSeen bool
	)
	appendText := func(value string) {
		if mediaSeen {
			wireMsg.Parts = append(wireMsg.Parts, wireContentPart{Type: "text", Text: value})
			return
		}
		text.WriteString(value)
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			appendText(value.Text)
		case message.ImagePart:
			if !entry.vision {
				ledger.reject(fields[message.PartImage], "model does not accept image input")
				continue
			}
			// Switch to ordered parts: any text gathered so far becomes the
			// leading text element so the wire keeps the canonical order.
			if !mediaSeen {
				mediaSeen = true
				if text.Len() > 0 {
					wireMsg.Parts = append(wireMsg.Parts, wireContentPart{Type: "text", Text: text.String()})
					text.Reset()
				}
			}
			wireMsg.Parts = append(wireMsg.Parts, wireContentPart{
				Type:     "image_url",
				ImageURL: &wireMediaURL{URL: imageValue(value.Source)},
			})
		case message.VideoPart:
			if !entry.video {
				ledger.reject(fields[message.PartVideo], "model does not accept video input")
				continue
			}
			if !mediaSeen {
				mediaSeen = true
				if text.Len() > 0 {
					wireMsg.Parts = append(wireMsg.Parts, wireContentPart{Type: "text", Text: text.String()})
					text.Reset()
				}
			}
			wireMsg.Parts = append(wireMsg.Parts, wireContentPart{
				Type:     "video_url",
				VideoURL: &wireMediaURL{URL: videoValue(value.Source)},
			})
		case message.AudioPart:
			ledger.reject(fields[message.PartAudio], "kimi models do not accept audio input")
		case message.FilePart:
			ledger.reject(fields[message.PartFile], "file references are not supported")
		case message.DataPart:
			appendText("\n" + string(value.Value) + "\n")
		case message.ToolCallPart:
			if role != "assistant" {
				ledger.reject(fields[message.PartToolCall], "tool calls belong to assistant context")
				continue
			}
			call := wireToolCall{ID: value.Call.ID, Type: "function"}
			call.Function.Name = value.Call.Name
			call.Function.Arguments = string(value.Call.Arguments)
			wireMsg.ToolCalls = append(wireMsg.ToolCalls, call)
		case message.ToolResultPart:
			ledger.reject(fields[message.PartToolResult], "tool results ride tool-role messages, not user or assistant turns")
		case message.ReasoningPart:
			if role != "assistant" {
				ledger.reject(fields[message.PartReasoning], "reasoning parts belong to assistant context")
				continue
			}
			switch {
			case entry.keepThinkingAlways, entry.keepThinking:
				wireMsg.Reasoning += value.Text
			default:
				ledger.drop(fields[message.PartReasoning], "model cannot re-ingest reasoning history")
			}
		}
	}
	if !mediaSeen {
		wireMsg.Text = text.String()
	}
	return wireMsg
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
			wire.Messages = append(wire.Messages, wireMessage{
				Role:       "tool",
				Text:       value.Result.Content,
				ToolCallID: value.Result.CallID,
			})
		default:
			ledger.reject(fields[part.Kind()], "tool-role content carries tool results only")
		}
	}
}

func compileIntent(wire *generateWire, intent inference.Intent, entry catalogEntry, ledger *ledger) {
	if intent.Image != nil {
		ledger.reject(inference.FieldGenerateIntentImage, "kimi models do not generate images")
	}
	if intent.Audio != nil {
		ledger.reject(inference.FieldGenerateIntentAudio, "kimi models do not generate audio")
	}
	if intent.Video != nil {
		ledger.reject(inference.FieldGenerateIntentVideo, "kimi models do not generate video")
	}
	text := intent.Text
	if text == nil {
		return
	}

	if response := text.Response; response != nil {
		switch response.Kind {
		case inference.ResponseText:
			wire.ResponseFormat = &wireResponseFormat{Type: "text"}
		case inference.ResponseJSONObject:
			wire.ResponseFormat = &wireResponseFormat{Type: "json_object"}
		case inference.ResponseJSONSchema:
			wire.ResponseFormat = &wireResponseFormat{
				Type: "json_schema",
				JSONSchema: &wireJSONSchemaFn{
					Name:   response.Name,
					Schema: response.Schema,
					Strict: true,
				},
			}
		}
	}
	if text.MaxOutputTokens != nil {
		maxTokens := int64(*text.MaxOutputTokens)
		wire.MaxTokens = &maxTokens
	}

	// Sampling knobs exist on the moonshot-v1 family only; the K3 / K2.x
	// request schemas carry none, so they drop with a reason there.
	if text.Temperature != nil {
		if entry.sampling {
			wire.Temperature = clonePointer(text.Temperature)
		} else {
			ledger.drop(inference.FieldGenerateIntentTemperature, "sampling knobs exist on moonshot-v1 only")
		}
	}
	if text.TopP != nil {
		if entry.sampling {
			wire.TopP = clonePointer(text.TopP)
		} else {
			ledger.drop(inference.FieldGenerateIntentTopP, "sampling knobs exist on moonshot-v1 only")
		}
	}

	for _, definition := range text.Tools {
		def := wireTool{Type: "function"}
		def.Function.Name = definition.Name
		def.Function.Description = definition.Description
		def.Function.Parameters = definition.InputSchema
		wire.Tools = append(wire.Tools, def)
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceAuto:
			wire.toolChoiceRaw = json.RawMessage(`"auto"`)
		case inference.ToolChoiceNone:
			wire.toolChoiceRaw = json.RawMessage(`"none"`)
		case inference.ToolChoiceRequired:
			wire.toolChoiceRaw = json.RawMessage(`"required"`)
		case inference.ToolChoiceNamed:
			named, err := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choice.Name},
			})
			if err != nil {
				ledger.reject(inference.FieldGenerateIntentToolChoiceKind, "tool choice name does not encode")
			} else {
				wire.toolChoiceRaw = named
			}
		}
	}

	compileReasoning(wire, text, entry, ledger)
}

// compileReasoning maps the canonical reasoning knobs onto the model
// family's dialect.
func compileReasoning(wire *generateWire, text *inference.TextIntent, entry catalogEntry, ledger *ledger) {
	if text.ReasoningEnabled != nil {
		switch {
		case !entry.reasoning:
			ledger.reject(inference.FieldGenerateIntentReasoningEnabled, "model has no thinking control")
		case entry.reasoningAlways && !*text.ReasoningEnabled:
			ledger.reject(inference.FieldGenerateIntentReasoningEnabled, "model always thinks; thinking cannot be disabled")
		case entry.reasoningAlways:
			// k3 / k2.7-code think unconditionally: an explicit true is a
			// no-op, no wire field needed.
		default:
			kind := "disabled"
			if *text.ReasoningEnabled {
				kind = "enabled"
			}
			wire.Thinking = &thinkingOn{Type: kind}
		}
	}
	if text.ReasoningEffort != "" {
		switch {
		case !entry.reasoning:
			ledger.reject(inference.FieldGenerateIntentReasoningEffort, "model has no thinking control")
		case !entry.reasoningEffort:
			ledger.drop(inference.FieldGenerateIntentReasoningEffort, "model's thinking is binary; no effort dial exists")
		default:
			switch text.ReasoningEffort {
			case inference.ReasoningLow:
				wire.Effort = "low"
			case inference.ReasoningMedium:
				wire.Effort = "high"
				ledger.drop(inference.FieldGenerateIntentReasoningEffort, "kimi-k3 quantizes medium effort to high")
			case inference.ReasoningHigh:
				wire.Effort = "high"
			}
		}
	}
}

// compileExtensions applies the provider extensions and derives the
// Preserved-Thinking default: kimi-k2.6 keeps history reasoning when the
// conversation carries a trace, unless the caller overrides.
func compileExtensions(
	wire *generateWire,
	options GenerateOptions,
	entry catalogEntry,
	sawHistoryReasoning bool,
	ledger *ledger,
) {
	wire.PromptCacheKey = options.PromptCacheKey
	if options.PreserveThinking != nil && !entry.keepThinking {
		ledger.reject(
			inference.ExtensionField("preserve_thinking").Qualify(options),
			"model has no Preserved-Thinking control",
		)
		return
	}
	keep := sawHistoryReasoning
	if options.PreserveThinking != nil {
		keep = *options.PreserveThinking
	}
	if keep && entry.keepThinking {
		if wire.Thinking == nil {
			wire.Thinking = &thinkingOn{Type: "enabled"}
		}
		wire.Thinking.Keep = "all"
		wire.PreserveKeep = true
	}
}

// imageValue renders one image input: URL, ms:// file id, or Base64 data
// URI for inline bytes.
func imageValue(source media.ImageSource) string {
	if source.Kind() == media.SourceInline {
		return "data:" + source.MediaType() + ";base64," +
			base64.StdEncoding.EncodeToString(source.Bytes())
	}
	return source.URL()
}

// videoValue renders one video input; Kimi accepts the same shapes as for
// images (URL or Base64 data URI).
func videoValue(source media.VideoSource) string {
	if source.Kind() == media.SourceInline {
		return "data:" + source.MediaType() + ";base64," +
			base64.StdEncoding.EncodeToString(source.Bytes())
	}
	return source.URL()
}

func ptrInt64(value int64) *int64 { return &value }
