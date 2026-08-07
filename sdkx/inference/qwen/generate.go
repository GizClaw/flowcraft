package qwen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// generateWire is the compiled request: the provider-owned intermediate
// representation every request passes through. It mirrors DashScope's
// native envelope — model, input.messages, parameters — one hop below the
// JSON boundary. The runtime requires wire types to be fully concrete (no
// open interfaces), so polymorphic fields ride custom marshalers on
// concrete structs.
type generateWire struct {
	Path       string         `json:"-"` // endpoint path (text- vs multimodal-generation)
	Model      string         `json:"-"`
	Messages   []wireMessage  `json:"-"`
	Parameters wireParameters `json:"-"`
}

func (w generateWire) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Model      string         `json:"model"`
		Input      wireInput      `json:"input"`
		Parameters wireParameters `json:"parameters"`
	}{w.Model, wireInput{w.Messages}, w.Parameters})
}

type wireInput struct {
	Messages []wireMessage `json:"messages"`
}

// wireMessage mirrors one DashScope message. Content is a string for
// text-generation models and an array of items on the multimodal endpoint;
// the marshaler picks the shape from whichever field is set.
type wireMessage struct {
	Role             string            `json:"role"`
	Text             string            `json:"-"`
	Items            []wireContentItem `json:"-"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

func (m wireMessage) MarshalJSON() ([]byte, error) {
	var content any
	if m.Items != nil {
		content = m.Items
	} else if m.Text != "" || len(m.ToolCalls) == 0 {
		content = m.Text
	}
	return json.Marshal(struct {
		Role             string         `json:"role"`
		Content          any            `json:"content,omitempty"`
		ReasoningContent string         `json:"reasoning_content,omitempty"`
		ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string         `json:"tool_call_id,omitempty"`
	}{m.Role, content, m.ReasoningContent, m.ToolCalls, m.ToolCallID})
}

// wireContentItem is one multimodal content item; exactly one field is set.
type wireContentItem struct {
	Text  *string `json:"text,omitempty"`
	Image string  `json:"image,omitempty"`
	Video string  `json:"video,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"`
	Index    *int         `json:"index,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireParameters is the DashScope parameters object; only set fields
// marshal.
type wireParameters struct {
	ResultFormat      string              `json:"result_format"`
	IncrementalOutput *bool               `json:"incremental_output,omitempty"`
	MaxTokens         *int                `json:"max_tokens,omitempty"`
	Temperature       *float64            `json:"temperature,omitempty"`
	TopP              *float64            `json:"top_p,omitempty"`
	TopK              *int64              `json:"top_k,omitempty"`
	EnableThinking    *bool               `json:"enable_thinking,omitempty"`
	ThinkingBudget    *int64              `json:"thinking_budget,omitempty"`
	PreserveThinking  *bool               `json:"preserve_thinking,omitempty"`
	ReasoningEffort   string              `json:"reasoning_effort,omitempty"`
	RepetitionPenalty *float64            `json:"repetition_penalty,omitempty"`
	PresencePenalty   *float64            `json:"presence_penalty,omitempty"`
	ResponseFormat    *wireResponseFormat `json:"response_format,omitempty"`
	Tools             []wireTool          `json:"tools,omitempty"`
	ToolChoice        *wireToolChoice     `json:"tool_choice,omitempty"`
}

type wireResponseFormat struct {
	Type string `json:"type"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolDefine `json:"function"`
}

type wireToolDefine struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireToolChoice marshals as the bare mode string for auto/none and as the
// function-targeting object for a named choice.
type wireToolChoice struct {
	Mode string
	Name string
}

func (c wireToolChoice) MarshalJSON() ([]byte, error) {
	if c.Name == "" {
		return json.Marshal(c.Mode)
	}
	return json.Marshal(struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}{Type: "function", Function: struct {
		Name string `json:"name"`
	}{c.Name}})
}

// compiler converts canonical generate requests into DashScope wire
// requests, one instance per compile.
type compiler struct {
	model   string
	entry   catalogEntry
	stream  bool
	ledger  *ledger
	options GenerateOptions
	wire    generateWire
}

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
		c := &compiler{
			model:  model,
			entry:  entry,
			stream: shape == inference.GenerateExecutionStream,
			ledger: newLedger(
				inference.OperationGenerate,
				request.ActiveFieldsFor(shape),
			),
		}
		options, other := operationExtensions[GenerateOptions](request.Extensions)
		c.options = options
		rejectOtherExtensions("generate", other, c.ledger)

		c.wire = generateWire{
			Model:      model,
			Parameters: wireParameters{ResultFormat: "message"},
		}
		if entry.multimodal() {
			c.wire.Path = pathMultimodalGeneration
		} else {
			c.wire.Path = pathTextGeneration
		}
		if c.stream {
			// Streams always answer incrementally: the canonical stream
			// surface is delta-shaped, never cumulative.
			incremental := true
			c.wire.Parameters.IncrementalOutput = &incremental
		}

		c.messages(request)
		c.textIntent(request)
		c.intents(request)
		c.extensions()

		report := c.ledger.report()
		if len(c.ledger.order) > 0 {
			return inference.Compiled[generateWire]{Report: report}, c.ledger.err()
		}
		return inference.Compiled[generateWire]{
			Wire:   c.wire,
			Report: report,
		}, nil
	}
}

// messages compiles context and input into DashScope's message array.
func (c *compiler) messages(request inference.GenerateRequest) {
	for _, turn := range request.Context {
		switch turn.Role {
		case message.RoleSystem:
			c.contextSystem(turn.Content.Parts)
		case message.RoleUser:
			c.wire.Messages = append(c.wire.Messages, c.userMessage(
				turn.Content.Parts, contextPartFields,
			))
		case message.RoleAssistant:
			c.wire.Messages = append(c.wire.Messages, c.assistantMessage(
				turn.Content.Parts, contextPartFields,
			))
		case message.RoleTool:
			c.toolMessages(turn.Content.Parts, contextPartFields)
		default:
			c.ledger.reject(
				inference.FieldGenerateContextRole,
				fmt.Sprintf("role %q is not a DashScope message role", turn.Role),
			)
		}
	}

	if request.Input.Role == inference.InputRoleTool {
		c.toolMessages(request.Input.Content.Parts, inputPartFields)
		return
	}
	c.wire.Messages = append(c.wire.Messages, c.userMessage(
		request.Input.Content.Parts, inputPartFields,
	))
}

// contextSystem compiles system context. Only text parts carry; any other
// part kind rejects.
func (c *compiler) contextSystem(parts []message.Part) {
	var texts []string
	for _, part := range parts {
		if text, ok := part.(message.TextPart); ok {
			texts = append(texts, text.Text)
			continue
		}
		c.ledger.reject(
			contextPartFields[part.Kind()],
			fmt.Sprintf("system context cannot carry %s parts", part.Kind()),
		)
	}
	if len(texts) > 0 {
		c.wire.Messages = append(c.wire.Messages, wireMessage{
			Role: "system",
			Text: strings.Join(texts, "\n\n"),
		})
	}
}

// userMessage compiles user-role parts: text plus, on multimodal entries,
// image and video items.
func (c *compiler) userMessage(
	parts []message.Part,
	partFields map[message.PartKind]inference.FieldID,
) wireMessage {
	var texts []string
	var items []wireContentItem
	for _, part := range parts {
		switch typed := part.(type) {
		case message.TextPart:
			texts = append(texts, typed.Text)
			text := typed.Text
			items = append(items, wireContentItem{Text: &text})
		case message.ImagePart:
			if !c.entry.vision {
				c.ledger.reject(
					partFields[message.PartImage],
					fmt.Sprintf("model %s does not accept image input", c.model),
				)
				continue
			}
			items = append(items, wireContentItem{Image: imageValue(typed.Source)})
		case message.VideoPart:
			if !c.entry.video {
				c.ledger.reject(
					partFields[message.PartVideo],
					fmt.Sprintf("model %s does not accept video input", c.model),
				)
				continue
			}
			value, ok := videoValue(typed.Source)
			if !ok {
				c.ledger.reject(
					partFields[message.PartVideo],
					"video input must be a URL; inline bytes are unsupported",
				)
				continue
			}
			items = append(items, wireContentItem{Video: value})
		default:
			if _, known := partFields[part.Kind()]; !known {
				continue
			}
			c.ledger.reject(
				partFields[part.Kind()],
				fmt.Sprintf("user input cannot carry %s parts", part.Kind()),
			)
		}
	}
	if c.entry.multimodal() {
		return wireMessage{Role: "user", Items: items}
	}
	return wireMessage{Role: "user", Text: strings.Join(texts, "\n\n")}
}

// imageValue renders one image input. DashScope takes a URL or a
// data:image/...;base64 URI for inline bytes.
func imageValue(source media.ImageSource) string {
	if source.Kind() == media.SourceInline {
		return "data:" + source.MediaType() + ";base64," +
			base64.StdEncoding.EncodeToString(source.Bytes())
	}
	return source.URL()
}

// videoValue renders one video input. DashScope takes a video file URL or
// a frame list URL; inline bytes have no surface.
func videoValue(source media.VideoSource) (string, bool) {
	if source.Kind() == media.SourceURL {
		return source.URL(), true
	}
	return "", false
}

// assistantMessage compiles assistant history: text content, tool calls,
// and reasoning traces. Reasoning round-trip rides preserve_thinking and
// only exists on entries that declare it; elsewhere the trace drops.
func (c *compiler) assistantMessage(
	parts []message.Part,
	partFields map[message.PartKind]inference.FieldID,
) wireMessage {
	var msg wireMessage
	msg.Role = "assistant"
	var texts []string
	var reasoning []string
	for _, part := range parts {
		switch typed := part.(type) {
		case message.TextPart:
			texts = append(texts, typed.Text)
		case message.ToolCallPart:
			msg.ToolCalls = append(msg.ToolCalls, wireToolCall{
				ID:   typed.Call.ID,
				Type: "function",
				Function: wireFunction{
					Name:      typed.Call.Name,
					Arguments: string(typed.Call.Arguments),
				},
			})
		case message.ReasoningPart:
			reasoning = append(reasoning, typed.Text)
		default:
			if _, known := partFields[part.Kind()]; !known {
				continue
			}
			c.ledger.reject(
				partFields[part.Kind()],
				fmt.Sprintf("assistant history cannot carry %s parts", part.Kind()),
			)
		}
	}
	msg.Text = strings.Join(texts, "\n\n")
	if trace := strings.Join(reasoning, "\n"); trace != "" {
		if c.entry.preserveThinking {
			msg.ReasoningContent = trace
		} else {
			c.ledger.drop(
				partFields[message.PartReasoning],
				fmt.Sprintf("model %s cannot re-ingest reasoning history; dropping trace", c.model),
			)
		}
	}
	return msg
}

// toolMessages compiles tool results into role=tool messages.
func (c *compiler) toolMessages(
	parts []message.Part,
	partFields map[message.PartKind]inference.FieldID,
) {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			if _, known := partFields[part.Kind()]; !known {
				continue
			}
			c.ledger.reject(
				partFields[part.Kind()],
				fmt.Sprintf("tool context cannot carry %s parts", part.Kind()),
			)
			continue
		}
		c.wire.Messages = append(c.wire.Messages, wireMessage{
			Role:       "tool",
			Text:       result.Result.Content,
			ToolCallID: result.Result.CallID,
		})
	}
}

// textIntent compiles the flattened text intent: response shaping, tools,
// sampling, and the reasoning switch.
func (c *compiler) textIntent(request inference.GenerateRequest) {
	text := request.Input.Content.Intent.Text
	if text == nil {
		return
	}
	c.responseFormat(text.Response)
	if text.MaxOutputTokens != nil {
		// max_tokens bounds the answer only (thinking excluded) — the
		// canonical semantic.
		maxTokens := *text.MaxOutputTokens
		c.wire.Parameters.MaxTokens = &maxTokens
	}
	c.tools(text)
	if text.Temperature != nil {
		temperature := *text.Temperature
		c.wire.Parameters.Temperature = &temperature
	}
	if text.TopP != nil {
		topP := *text.TopP
		c.wire.Parameters.TopP = &topP
	}
	c.reasoning(text)
}

func (c *compiler) responseFormat(format *inference.ResponseFormat) {
	if format == nil {
		return
	}
	switch format.Kind {
	case inference.ResponseText:
	case inference.ResponseJSONObject:
		c.wire.Parameters.ResponseFormat = &wireResponseFormat{Type: "json_object"}
	case inference.ResponseJSONSchema:
		c.ledger.reject(
			inference.FieldGenerateIntentTextResponseSchema,
			"response_format supports text and json_object only; no schema surface",
		)
	}
}

func (c *compiler) tools(text *inference.TextIntent) {
	if len(text.Tools) > 0 {
		definitions := make([]wireTool, 0, len(text.Tools))
		for _, definition := range text.Tools {
			definitions = append(definitions, wireTool{
				Type: "function",
				Function: wireToolDefine{
					Name:        definition.Name,
					Description: definition.Description,
					Parameters:  definition.InputSchema,
				},
			})
		}
		c.wire.Parameters.Tools = definitions
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceAuto:
			c.wire.Parameters.ToolChoice = &wireToolChoice{Mode: "auto"}
		case inference.ToolChoiceNone:
			c.wire.Parameters.ToolChoice = &wireToolChoice{Mode: "none"}
		case inference.ToolChoiceNamed:
			c.wire.Parameters.ToolChoice = &wireToolChoice{Name: choice.Name}
		case inference.ToolChoiceRequired:
			c.ledger.reject(
				inference.FieldGenerateIntentToolChoiceKind,
				"DashScope has no required tool choice (auto, none, or named)",
			)
		}
	}
}

// reasoning compiles the reasoning switch and effort. The effort levels
// exist only on qwen3.8-max-preview (reasoningEffort); other thinking
// models take thinking_budget through the extension instead, so an
// explicit effort drops with a reason. Thinking mode is stream-only on
// the commercial thinking models, so a unary compile with thinking on
// rejects the switch field itself.
func (c *compiler) reasoning(text *inference.TextIntent) {
	if text.ReasoningEnabled != nil {
		if !c.entry.reasoning {
			c.ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				fmt.Sprintf("model %s has no thinking mode", c.model),
			)
		} else {
			if *text.ReasoningEnabled && c.entry.thinkingStreamOnly && !c.stream {
				c.ledger.reject(
					inference.FieldGenerateIntentReasoningEnabled,
					fmt.Sprintf("model %s answers thinking requests on streams only", c.model),
				)
			}
			enabled := *text.ReasoningEnabled
			c.wire.Parameters.EnableThinking = &enabled
		}
	}
	if text.ReasoningEffort != "" {
		switch {
		case !c.entry.reasoning:
			c.ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				fmt.Sprintf("model %s has no thinking mode", c.model),
			)
		case !c.entry.reasoningEffort:
			c.ledger.drop(
				inference.FieldGenerateIntentReasoningEffort,
				fmt.Sprintf("model %s has no effort levels; bound the trace with the thinking_budget extension", c.model),
			)
		default:
			if level, ok := effortLevel(text.ReasoningEffort); ok {
				c.wire.Parameters.ReasoningEffort = level
			} else {
				c.ledger.reject(
					inference.FieldGenerateIntentReasoningEffort,
					fmt.Sprintf("reasoning effort %q is not a DashScope level", text.ReasoningEffort),
				)
			}
		}
	}
}

// effortLevel maps canonical effort onto DashScope's low/medium/xhigh
// scale: high lands on xhigh (DashScope's top level).
func effortLevel(effort inference.ReasoningEffort) (string, bool) {
	switch effort {
	case inference.ReasoningLow:
		return "low", true
	case inference.ReasoningMedium:
		return "medium", true
	case inference.ReasoningHigh:
		return "xhigh", true
	}
	return "", false
}

// intents rejects the non-text modality intents: this package serves
// generation only, media output lives in dedicated DashScope SKUs.
func (c *compiler) intents(request inference.GenerateRequest) {
	intent := request.Input.Content.Intent
	if intent.Image != nil {
		c.ledger.reject(
			inference.FieldGenerateIntentImage,
			"image generation is a dedicated DashScope SKU, not generation",
		)
	}
	if intent.Audio != nil {
		c.ledger.reject(
			inference.FieldGenerateIntentAudio,
			"speech output is a dedicated DashScope SKU, not generation",
		)
	}
	if intent.Video != nil {
		c.ledger.reject(
			inference.FieldGenerateIntentVideo,
			"video generation is a dedicated DashScope SKU, not generation",
		)
	}
}

// extensions compiles GenerateOptions onto parameters.
func (c *compiler) extensions() {
	o := c.options
	if o.ThinkingBudget != nil {
		if !c.entry.reasoning {
			c.ledger.reject(
				inference.ExtensionField("thinking_budget").Qualify(o),
				fmt.Sprintf("model %s has no thinking budget", c.model),
			)
		} else {
			budget := *o.ThinkingBudget
			c.wire.Parameters.ThinkingBudget = &budget
		}
	}
	if o.PreserveThinking != nil {
		preserve := *o.PreserveThinking
		c.wire.Parameters.PreserveThinking = &preserve
	}
	if o.TopK != nil {
		topK := *o.TopK
		c.wire.Parameters.TopK = &topK
	}
	if o.RepetitionPenalty != nil {
		penalty := *o.RepetitionPenalty
		c.wire.Parameters.RepetitionPenalty = &penalty
	}
	if o.PresencePenalty != nil {
		penalty := *o.PresencePenalty
		c.wire.Parameters.PresencePenalty = &penalty
	}
}

// defaultPreserveThinking applies the round-trip default: preserve_thinking
// turns on when the compiled history carries a reasoning trace and the
// extension left the switch unset.
func defaultPreserveThinking(wire *generateWire) {
	if wire.Parameters.PreserveThinking != nil {
		return
	}
	for _, message := range wire.Messages {
		if message.ReasoningContent != "" {
			preserve := true
			wire.Parameters.PreserveThinking = &preserve
			return
		}
	}
}

// dashUsage mirrors the usage object; ReasoningTokens lives under
// output_tokens_details on the wire.
type dashUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	ReasoningTokens int64 `json:"-"`
}

func (u dashUsage) canonical() inference.Usage {
	usage := inference.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.ReasoningTokens > 0 {
		reasoning := u.ReasoningTokens
		usage.Output.ReasoningTokens = &reasoning
	}
	return usage
}

// dashResponse is the generation envelope (unary body and SSE chunk share
// the shape).
type dashResponse struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Output    struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []wireToolCall  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"output"`
	Usage struct {
		InputTokens   int64 `json:"input_tokens"`
		OutputTokens  int64 `json:"output_tokens"`
		TotalTokens   int64 `json:"total_tokens"`
		OutputDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (r dashResponse) usage() dashUsage {
	return dashUsage{
		InputTokens:     r.Usage.InputTokens,
		OutputTokens:    r.Usage.OutputTokens,
		TotalTokens:     r.Usage.TotalTokens,
		ReasoningTokens: r.Usage.OutputDetails.ReasoningTokens,
	}
}

// messageContentText renders a message content payload: a plain string on
// text models, an array of {"text": ...} items on multimodal ones.
func messageContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain
	}
	var items []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		b.WriteString(item.Text)
	}
	return b.String()
}

func finishReason(raw string) inference.FinishReason {
	switch raw {
	case "stop":
		return inference.FinishCompleted
	case "length":
		return inference.FinishMaxOutput
	case "tool_calls":
		return inference.FinishToolCalls
	case "null", "":
		return ""
	}
	return inference.FinishOther
}

// decode builds the canonical response from one unary envelope.
func decodeGenerate(
	_ context.Context,
	raw dashResponse,
) (inference.GenerateResponse, error) {
	var response inference.GenerateResponse
	if len(raw.Output.Choices) == 0 {
		return inference.GenerateResponse{}, fmt.Errorf(
			"qwen: response carries no choices",
		)
	}
	choice := raw.Output.Choices[0]
	wireMsg := choice.Message
	var parts []message.Part
	if wireMsg.ReasoningContent != "" {
		parts = append(parts, message.ReasoningPart{Text: wireMsg.ReasoningContent})
	}
	if text := messageContentText(wireMsg.Content); text != "" {
		parts = append(parts, message.TextPart{Text: text})
	}
	for _, call := range wireMsg.ToolCalls {
		arguments := call.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			// No-argument tools answer with an empty string; canonical
			// tool calls require a JSON object.
			arguments = "{}"
		}
		parts = append(parts, message.ToolCallPart{Call: message.Call{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage(arguments),
		}})
	}
	response.Message = message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: parts},
	}
	response.FinishReason = finishReason(choice.FinishReason)
	response.Usage = raw.usage().canonical()
	response.Metadata.RequestID = raw.RequestID
	return response, nil
}

// transportGenerate executes one compiled unary request.
func transportGenerate(
	client *dashClient,
) inference.Transport[generateWire, dashResponse] {
	return func(ctx context.Context, wire generateWire) (dashResponse, error) {
		defaultPreserveThinking(&wire)
		var envelope dashResponse
		if err := client.postJSON(ctx, wire.Path, wire, &envelope); err != nil {
			return dashResponse{}, err
		}
		if err := classifyEnvelope(
			envelope.Code, envelope.Message, envelope.RequestID,
		); err != nil {
			return dashResponse{}, err
		}
		return envelope, nil
	}
}
