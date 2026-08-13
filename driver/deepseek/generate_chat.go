package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// chatWire is the provider-owned intermediate representation for one chat
// completion request. DeepSeek-only knobs (thinking, reasoning_effort)
// become per-request JSON overrides at the transport stage.
type chatWire struct {
	model       string
	messages    []wireChatMessage
	maxTokens   *int64
	temperature *float64
	topP        *float64
	jsonObject  bool
	tools       []wireTool
	toolChoice  *wireToolChoice
	effort      string
	thinking    *bool
	stream      bool
}

type wireChatMessage struct {
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

// compileChatGenerate compiles a canonical generate request into the chat
// completions wire model. System messages stay messages (the protocol has
// no instructions slot); user and assistant turns map one-to-one; tool
// results become tool-role messages.
func compileChatGenerate(
	model string,
	entry catalogEntry,
) inference.GenerateCompiler[chatWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[chatWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := chatWire{
			model:  model,
			stream: shape == inference.GenerateExecutionStream,
		}

		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				compileChatSystemMessage(&wire, turn.Content.Parts, ledger)
			case message.RoleTool:
				compileChatToolResults(&wire, turn.Content.Parts, contextPartFields, ledger)
			default:
				compileChatMessage(
					&wire,
					string(turn.Role),
					turn.Content.Parts,
					entry,
					contextPartFields,
					ledger,
				)
			}
		}
		if request.Input.Role == inference.InputRoleTool {
			compileChatToolResults(&wire, request.Input.Content.Parts, inputPartFields, ledger)
		} else {
			compileChatMessage(
				&wire,
				"user",
				request.Input.Content.Parts,
				entry,
				inputPartFields,
				ledger,
			)
		}
		compileChatIntent(&wire, request.Input.Content.Intent, entry, ledger)

		options, other := operationExtensions[GenerateOptions](request.Extensions)
		rejectOtherExtensions("generate", other, ledger)
		if options.WebSearch != nil {
			ledger.reject(
				inference.ExtensionField("web_search").Qualify(options),
				"chat completions does not support hosted web search",
			)
		}

		report := ledger.report()
		if err := ledger.err(); err != nil {
			return inference.Compiled[chatWire]{Report: report}, err
		}
		return inference.Compiled[chatWire]{Wire: wire, Report: report}, nil
	}
}

func compileChatSystemMessage(
	wire *chatWire,
	parts []message.Part,
	ledger *ledger,
) {
	var text strings.Builder
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			text.WriteString(value.Text)
		case message.DataPart:
			text.WriteString("\n" + string(value.Value) + "\n")
		default:
			ledger.reject(
				contextPartFields[part.Kind()],
				"system messages carry text only",
			)
		}
	}
	wire.messages = append(wire.messages, wireChatMessage{
		role: "system",
		text: text.String(),
	})
}

// compileChatMessage appends one chat message per canonical message.
// Assistant turns are where DeepSeek's reasoning round-trip rule applies:
// a turn that performed tool calls must carry its reasoning_content back,
// a turn that did not has no channel for it.
func compileChatMessage(
	wire *chatWire,
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
			text.WriteString("\n" + string(value.Value) + "\n")
		case message.ToolCallPart:
			toolCalls = append(toolCalls, wireToolCall{
				id:   value.Call.ID,
				name: value.Call.Name,
				args: bytesClone(value.Call.Arguments),
			})
		case message.ToolResultPart:
			ledger.reject(
				fields[message.PartToolResult],
				"tool results ride tool-role messages, not user or assistant turns",
			)
		case message.ReasoningPart:
			if role != "assistant" {
				ledger.reject(
					fields[message.PartReasoning],
					"reasoning parts belong to assistant context",
				)
				continue
			}
			if value.Text == "" {
				ledger.drop(
					fields[message.PartReasoning],
					"deepseek chat requires plain reasoning text",
				)
				continue
			}
			sawReasoning = true
			reasoning.WriteString(value.Text)
		}
	}

	wireMsg := wireChatMessage{
		role:      role,
		text:      text.String(),
		toolCalls: toolCalls,
	}
	if sawReasoning {
		if len(toolCalls) > 0 {
			wireMsg.reasoning = reasoning.String()
			wireMsg.hasReasoning = true
		} else {
			ledger.drop(
				fields[message.PartReasoning],
				"deepseek ignores reasoning on turns without tool calls",
			)
		}
	}
	wire.messages = append(wire.messages, wireMsg)
}

func compileChatToolResults(
	wire *chatWire,
	parts []message.Part,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		switch value := part.(type) {
		case message.ToolResultPart:
			wire.messages = append(wire.messages, wireChatMessage{
				role:   "tool",
				text:   value.Result.Content,
				callID: value.Result.CallID,
			})
		default:
			ledger.reject(
				fields[part.Kind()],
				"tool-role content carries tool results only",
			)
		}
	}
}

func compileChatIntent(
	wire *chatWire,
	intent inference.Intent,
	entry catalogEntry,
	ledger *ledger,
) {
	if text := intent.Text; text != nil {
		if response := text.Response; response != nil {
			switch response.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject:
				wire.jsonObject = true
			case inference.ResponseJSONSchema:
				ledger.reject(
					inference.FieldGenerateIntentTextResponseKind,
					"deepseek chat supports json_object responses only, not schema-constrained output",
				)
			}
		}
		if text.MaxOutputTokens != nil {
			max := int64(*text.MaxOutputTokens)
			wire.maxTokens = &max
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
			parameters:  bytesClone(definition.InputSchema),
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
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking control",
			)
		} else {
			wire.thinking = clonePointer(text.ReasoningEnabled)
		}
	}
	if text.ReasoningEffort != "" {
		if !entry.reasoning {
			ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no thinking control",
			)
		} else {
			// DeepSeek documents low/medium as aliases for high and
			// xhigh for max: pass the canonical effort through verbatim
			// and let the API normalize it.
			wire.effort = string(text.ReasoningEffort)
		}
	}
}

// wireToChatParams converts the wire model into openai-go chat completion
// params plus the per-request JSON overrides DeepSeek owns (thinking,
// reasoning_effort): the SDK does not type them, so they ride
// option.WithJSONSet.
func wireToChatParams(
	wire chatWire,
) (openaigo.ChatCompletionNewParams, []option.RequestOption) {
	params := openaigo.ChatCompletionNewParams{
		Model:    wire.model,
		Messages: wireChatMessagesToParams(wire.messages),
	}
	if wire.maxTokens != nil {
		params.MaxTokens = openaigo.Int(*wire.maxTokens)
	}
	if wire.temperature != nil {
		params.Temperature = openaigo.Float(*wire.temperature)
	}
	if wire.topP != nil {
		params.TopP = openaigo.Float(*wire.topP)
	}
	if wire.jsonObject {
		params.ResponseFormat = openaigo.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openaigo.ResponseFormatJSONObjectParam{},
		}
	}
	for _, definition := range wire.tools {
		params.Tools = append(params.Tools, openaigo.ChatCompletionToolUnionParam{
			OfFunction: &openaigo.ChatCompletionFunctionToolParam{
				Function: openaigo.FunctionDefinitionParam{
					Name:        definition.name,
					Description: openaigo.String(definition.description),
					Parameters:  openaigo.FunctionParameters(jsonMap(definition.parameters)),
				},
			},
		})
	}
	if choice := wire.toolChoice; choice != nil {
		switch choice.mode {
		case "auto", "none", "required":
			params.ToolChoice = openaigo.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openaigo.String(choice.mode),
			}
		case "named":
			params.ToolChoice = openaigo.ChatCompletionToolChoiceOptionUnionParam{
				OfFunctionToolChoice: &openaigo.ChatCompletionNamedToolChoiceParam{
					Function: openaigo.ChatCompletionNamedToolChoiceFunctionParam{
						Name: choice.name,
					},
				},
			}
		}
	}
	if wire.stream {
		params.StreamOptions = openaigo.ChatCompletionStreamOptionsParam{
			IncludeUsage: openaigo.Bool(true),
		}
	}

	var overrides []option.RequestOption
	if wire.thinking != nil {
		kind := "disabled"
		if *wire.thinking {
			kind = "enabled"
		}
		overrides = append(overrides, option.WithJSONSet(
			"thinking",
			map[string]any{"type": kind},
		))
	}
	if wire.effort != "" {
		overrides = append(overrides, option.WithJSONSet(
			"reasoning_effort",
			wire.effort,
		))
	}
	return params, overrides
}

func wireChatMessagesToParams(
	messages []wireChatMessage,
) []openaigo.ChatCompletionMessageParamUnion {
	out := make([]openaigo.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, message := range messages {
		switch message.role {
		case "system":
			out = append(out, openaigo.SystemMessage(message.text))
		case "tool":
			out = append(out, openaigo.ToolMessage(message.text, message.callID))
		case "assistant":
			var assistant openaigo.ChatCompletionAssistantMessageParam
			if message.text != "" {
				assistant.Content.OfString = openaigo.String(message.text)
			}
			for _, call := range message.toolCalls {
				assistant.ToolCalls = append(
					assistant.ToolCalls,
					openaigo.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openaigo.ChatCompletionMessageFunctionToolCallParam{
							ID: call.id,
							Function: openaigo.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      call.name,
								Arguments: string(call.args),
							},
						},
					},
				)
			}
			// DeepSeek 400s a thinking-mode request whose tool-calling
			// assistant turns lack reasoning_content, so a turn with tool
			// calls but no trace still carries the field — empty, which
			// the API accepts.
			if message.hasReasoning {
				assistant.SetExtraFields(map[string]any{
					"reasoning_content": message.reasoning,
				})
			} else if len(message.toolCalls) > 0 {
				assistant.SetExtraFields(map[string]any{
					"reasoning_content": "",
				})
			}
			out = append(
				out,
				openaigo.ChatCompletionMessageParamUnion{OfAssistant: &assistant},
			)
		default:
			out = append(out, openaigo.UserMessage(message.text))
		}
	}
	return out
}

// transportChatGenerate executes the compiled request against the chat
// completions endpoint.
func transportChatGenerate(
	client openaigo.Client,
) inference.Transport[chatWire, generateRaw] {
	return func(ctx context.Context, wire chatWire) (generateRaw, error) {
		params, overrides := wireToChatParams(wire)
		response, err := client.Chat.Completions.New(ctx, params, overrides...)
		if err != nil {
			return generateRaw{}, classifyError(err)
		}
		return chatCompletionToRaw(response)
	}
}

func chatCompletionToRaw(
	response *openaigo.ChatCompletion,
) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, errdefs.NotAvailablef(
			"deepseek: nil response with no error (provider misbehaviour)")
	}
	if len(response.Choices) == 0 {
		return generateRaw{}, errdefs.NotAvailablef(
			"deepseek: response carries no choices")
	}
	choice := response.Choices[0]
	finish, err := mapFinishReason(choice.FinishReason)
	if err != nil {
		return generateRaw{}, err
	}

	raw := generateRaw{
		id:     response.ID,
		finish: finish,
		usage:  usageToRaw(response.Usage),
	}
	if reasoning := reasoningContentOf(choice.Message.JSON.ExtraFields); reasoning != "" {
		raw.reasonings = append(raw.reasonings, rawReasoning{text: reasoning})
	}
	if choice.Message.Content != "" {
		raw.texts = append(raw.texts, choice.Message.Content)
	}
	for _, call := range choice.Message.ToolCalls {
		function := call.AsFunction()
		raw.toolCalls = append(raw.toolCalls, rawToolCall{
			id:   function.ID,
			name: function.Function.Name,
			args: []byte(function.Function.Arguments),
		})
	}
	return raw, nil
}

// reasoningContentOf extracts the DeepSeek-owned reasoning_content extra
// from a message's raw JSON. The SDK does not type it — and marks extras
// invalid because the type is unverifiable, so presence plus a non-empty
// raw payload is the presence check, not Valid().
func reasoningContentOf(extras map[string]respjson.Field) string {
	field, exists := extras["reasoning_content"]
	if !exists || field.Raw() == "" {
		return ""
	}
	var reasoning string
	if err := json.Unmarshal([]byte(field.Raw()), &reasoning); err != nil {
		return ""
	}
	return reasoning
}

func usageToRaw(usage openaigo.CompletionUsage) rawUsage {
	raw := rawUsage{
		input:     usage.PromptTokens,
		output:    usage.CompletionTokens,
		total:     usage.TotalTokens,
		reasoning: usage.CompletionTokensDetails.ReasoningTokens,
		present:   usage.JSON.CompletionTokens.Valid() || usage.JSON.PromptTokens.Valid(),
	}
	// DeepSeek reports the prompt cache hit as a top-level usage field the
	// SDK does not type; OpenAI-style prompt_tokens_details.cached_tokens
	// stays zero on this surface.
	if field, exists := usage.JSON.ExtraFields["prompt_cache_hit_tokens"]; exists && field.Raw() != "" {
		var cached int64
		if err := json.Unmarshal([]byte(field.Raw()), &cached); err == nil {
			raw.cached = cached
		}
	}
	// DeepSeek reports the uncached (freshly processed) input as
	// prompt_cache_miss_tokens alongside the hit counter.
	if field, exists := usage.JSON.ExtraFields["prompt_cache_miss_tokens"]; exists && field.Raw() != "" {
		var uncached int64
		if err := json.Unmarshal([]byte(field.Raw()), &uncached); err == nil {
			raw.uncached = uncached
		}
	}
	return raw
}

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

// chatStream adapts the SDK's SSE stream to the provider stream contract.
// It assigns canonical part indices as deltas arrive (reasoning precedes
// text on this surface), folds chunk fields into delta events, and holds
// the finish event back until the stream ends so usage rides along —
// DeepSeek delivers usage in its own chunk after the finish_reason chunk.
type chatStream struct {
	stream *ssestream.Stream[openaigo.ChatCompletionChunk]

	pending []streamRaw

	reasoningPart int
	textPart      int
	toolParts     map[int64]int
	nextPart      int

	finish    inference.FinishReason
	finishErr error
	usage     *rawUsage
	sawTools  bool
	ended     bool
	id        string
}

// transportChatGenerateStream opens the streaming request and returns the
// stateful provider stream.
func transportChatGenerateStream(
	client openaigo.Client,
) inference.Transport[chatWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire chatWire,
	) (inference.ProviderStream[streamRaw], error) {
		params, overrides := wireToChatParams(wire)
		stream := client.Chat.Completions.NewStreaming(ctx, params, overrides...)
		if stream == nil {
			return nil, errdefs.NotAvailablef(
				"deepseek: nil stream handle (provider misbehaviour)")
		}
		if err := stream.Err(); err != nil {
			return nil, classifyError(err)
		}
		return &chatStream{
			stream:        stream,
			reasoningPart: -1,
			textPart:      -1,
			toolParts:     make(map[int64]int),
		}, nil
	}
}

func (s *chatStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return classifyError(s.stream.Close())
}

func (s *chatStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for len(s.pending) == 0 {
		if s.ended {
			if s.finishErr != nil {
				err := s.finishErr
				s.finishErr = nil
				return streamRaw{}, err
			}
			return streamRaw{}, io.EOF
		}
		if !s.stream.Next() {
			if err := s.stream.Err(); err != nil {
				return streamRaw{}, classifyError(err)
			}
			s.end()
			continue
		}
		s.apply(s.stream.Current())
		if err := ctx.Err(); err != nil {
			return streamRaw{}, errdefs.FromContext(err)
		}
	}
	event := s.pending[0]
	s.pending = s.pending[1:]
	return event, nil
}

// apply folds one chunk into zero or more delta events. Finish reasons and
// usage are recorded, not emitted: the finish event ships at stream end so
// it carries both.
func (s *chatStream) apply(chunk openaigo.ChatCompletionChunk) {
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Usage.JSON.PromptTokens.Valid() || chunk.Usage.JSON.CompletionTokens.Valid() {
		usage := usageToRaw(chunk.Usage)
		s.usage = &usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	if reasoning := reasoningContentOf(delta.JSON.ExtraFields); reasoning != "" {
		s.pending = append(s.pending, streamRaw{
			kind: streamRawReasoning,
			part: s.reasoningIndex(),
			text: reasoning,
		})
	}
	if delta.Content != "" {
		s.pending = append(s.pending, streamRaw{
			kind: streamRawText,
			part: s.textIndex(),
			text: delta.Content,
		})
	}
	for _, call := range delta.ToolCalls {
		part, exists := s.toolParts[call.Index]
		if !exists {
			part = s.assignPart()
			s.toolParts[call.Index] = part
			s.sawTools = true
		}
		s.pending = append(s.pending, streamRaw{
			kind: streamRawToolFragment,
			part: part,
			tool: streamRawTool{
				id:           call.ID,
				name:         call.Function.Name,
				argsFragment: call.Function.Arguments,
			},
		})
	}
	if choice.FinishReason != "" {
		finish, err := mapFinishReason(choice.FinishReason)
		if err != nil {
			s.finishErr = err
		} else {
			s.finish = finish
		}
	}
}

// end emits the terminal event exactly once: the recorded finish reason
// (defaulting to completed, or tool_calls when calls streamed without an
// explicit reason) plus the usage chunk's accounting.
func (s *chatStream) end() {
	if s.ended {
		return
	}
	s.ended = true
	if s.finishErr != nil {
		return
	}
	finish := s.finish
	if finish == "" && s.sawTools {
		finish = inference.FinishToolCalls
	}
	if finish == "" {
		finish = inference.FinishCompleted
	}
	s.pending = append(s.pending, streamRaw{
		kind:       streamRawFinish,
		finish:     finish,
		usage:      s.usage,
		responseID: s.id,
	})
}

func (s *chatStream) assignPart() int {
	part := s.nextPart
	s.nextPart++
	return part
}

func (s *chatStream) reasoningIndex() int {
	if s.reasoningPart < 0 {
		s.reasoningPart = s.assignPart()
	}
	return s.reasoningPart
}

func (s *chatStream) textIndex() int {
	if s.textPart < 0 {
		s.textPart = s.assignPart()
	}
	return s.textPart
}
