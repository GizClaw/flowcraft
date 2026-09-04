package openai

import (
	"context"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"
)

// wireToChatParams converts the shared generate wire into openai-go chat
// completion params. Chat Completions cannot carry Responses-style
// reasoning items or the hosted web_search tool; the compiler already
// rejects web_search in chat mode and reasoning items are dropped here
// (they cannot round-trip on this surface).
func wireToChatParams(wire generateWire) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(wire.model),
		Messages: chatMessages(wire.items),
	}
	if wire.maxTokens != nil {
		params.MaxCompletionTokens = param.NewOpt(*wire.maxTokens)
	}
	if wire.temperature != nil {
		params.Temperature = param.NewOpt(*wire.temperature)
	}
	if wire.topP != nil {
		params.TopP = param.NewOpt(*wire.topP)
	}
	if wire.reasoning != "" {
		params.ReasoningEffort = shared.ReasoningEffort(wire.reasoning)
	}
	if wire.textFormat != nil {
		params.ResponseFormat = chatTextFormat(wire.textFormat)
	}
	for _, definition := range wire.tools {
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: openai.FunctionDefinitionParam{
					Name:        definition.name,
					Description: openai.String(definition.description),
					Parameters:  openai.FunctionParameters(schemaMap(definition.schema)),
				},
			},
		})
	}
	if choice := wire.toolChoice; choice != nil {
		params.ToolChoice = chatToolChoice(choice)
	}
	if wire.stream {
		options := openai.ChatCompletionStreamOptionsParam{}
		set := false
		if wire.chatStreamIncludeUsage {
			options.IncludeUsage = openai.Bool(true)
			set = true
		}
		if obfuscation := wire.chatStreamIncludeObfuscation; obfuscation != nil {
			options.IncludeObfuscation = openai.Bool(*obfuscation)
			set = true
		}
		if set {
			params.StreamOptions = options
		}
	}
	if wire.requestMetadataEnvelope == "metadata" && len(wire.requestMetadata) > 0 {
		params.Metadata = wire.requestMetadata
	}
	return params
}

// chatMessages assembles chat messages from the Responses-style wire
// items. Tool calls are attached to the assistant message that precedes
// them, and tool results become tool-role messages, matching the chat
// completions conversation shape.
func chatMessages(items []wireItem) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(items))
	var assistant *openai.ChatCompletionAssistantMessageParam
	flushAssistant := func() {
		if assistant == nil {
			return
		}
		out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: assistant})
		assistant = nil
	}
	for _, item := range items {
		switch item.kind {
		case wireItemMessage:
			flushAssistant()
			switch item.role {
			case "system":
				out = append(out, openai.SystemMessage(chatText(item.content)))
			case "assistant":
				assistant = &openai.ChatCompletionAssistantMessageParam{}
				if text := chatText(item.content); text != "" {
					assistant.Content.OfString = openai.String(text)
				}
			default: // user
				parts := chatUserContent(item.content)
				if len(parts) == 1 && parts[0].OfText != nil {
					out = append(out, openai.UserMessage(parts[0].OfText.Text))
				} else {
					out = append(out, openai.UserMessage(parts))
				}
			}
		case wireItemToolCall:
			if assistant == nil {
				assistant = &openai.ChatCompletionAssistantMessageParam{}
			}
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: item.callID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      item.name,
						Arguments: string(item.args),
					},
				},
			})
		case wireItemToolResult:
			flushAssistant()
			out = append(out, openai.ToolMessage(item.output, item.callID))
		case wireItemReasoning:
			// Chat Completions has no standardized encrypted reasoning
			// round-trip; the trace is intentionally dropped.
			continue
		}
	}
	flushAssistant()
	return out
}

func chatText(content []wireContent) string {
	var builder strings.Builder
	for _, part := range content {
		if part.kind == wireContentText {
			builder.WriteString(part.text)
		}
	}
	return builder.String()
}

func chatUserContent(content []wireContent) []openai.ChatCompletionContentPartUnionParam {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(content))
	for _, part := range content {
		switch part.kind {
		case wireContentText:
			parts = append(parts, openai.ChatCompletionContentPartUnionParam{
				OfText: &openai.ChatCompletionContentPartTextParam{Text: part.text},
			})
		case wireContentImage:
			parts = append(parts, openai.ChatCompletionContentPartUnionParam{
				OfImageURL: &openai.ChatCompletionContentPartImageParam{
					ImageURL: openai.ChatCompletionContentPartImageImageURLParam{
						URL: part.uri,
					},
				},
			})
		}
	}
	return parts
}

func chatTextFormat(format *wireTextFormat) openai.ChatCompletionNewParamsResponseFormatUnion {
	switch format.kind {
	case "json_object":
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	case "json_schema":
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   format.name,
					Strict: param.NewOpt(format.strict),
					Schema: schemaMap(format.schema),
				},
			},
		}
	}
	return openai.ChatCompletionNewParamsResponseFormatUnion{}
}

func chatToolChoice(choice *wireToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
	switch choice.mode {
	case "none", "required":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String(choice.mode),
		}
	case "named":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: choice.name},
			},
		}
	default:
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String("auto"),
		}
	}
}

// transportChatGenerate executes the compiled request against the chat
// completions endpoint.
func transportChatGenerate(client openai.Client) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		response, err := client.Chat.Completions.New(
			ctx,
			wireToChatParams(wire),
			requestMetadataOptions(wire)...,
		)
		if err != nil {
			classified := classifyError(err)
			logInferenceCall(ctx, "generate", wire.model, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := chatCompletionToRaw(response)
		if err != nil {
			logInferenceCall(ctx, "generate", wire.model, err, "", "")
			return generateRaw{}, err
		}
		logInferenceCall(ctx, "generate", wire.model, nil, "", raw.id)
		return raw, nil
	}
}

func chatCompletionToRaw(response *openai.ChatCompletion) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, errdefs.NotAvailablef(
			"openai: nil chat completion response (provider misbehaviour)")
	}
	if len(response.Choices) == 0 {
		return generateRaw{}, errdefs.NotAvailablef(
			"openai: chat completion response carries no choices")
	}
	choice := response.Choices[0]
	finish, err := chatFinishReason(choice.FinishReason)
	if err != nil {
		return generateRaw{}, err
	}
	raw := generateRaw{
		id:     response.ID,
		finish: finish,
		usage:  chatUsageToRaw(response.Usage),
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

func chatFinishReason(reason string) (inference.FinishReason, error) {
	switch reason {
	case "", "stop":
		return inference.FinishCompleted, nil
	case "length":
		return inference.FinishMaxOutput, nil
	case "tool_calls":
		return inference.FinishToolCalls, nil
	case "content_filter":
		return inference.FinishContentFilter, nil
	default:
		return inference.FinishOther, nil
	}
}

func chatUsageToRaw(usage openai.CompletionUsage) rawUsage {
	return rawUsage{
		inputTokens:              usage.PromptTokens,
		outputTokens:             usage.CompletionTokens,
		totalTokens:              usage.TotalTokens,
		cachedTokens:             usage.PromptTokensDetails.CachedTokens,
		cacheWriteTokens:         usage.PromptTokensDetails.CacheWriteTokens,
		reasoningTokens:          usage.CompletionTokensDetails.ReasoningTokens,
		acceptedPredictionTokens: usage.CompletionTokensDetails.AcceptedPredictionTokens,
		rejectedPredictionTokens: usage.CompletionTokensDetails.RejectedPredictionTokens,
		inputAudioTokens:         usage.PromptTokensDetails.AudioTokens,
		outputAudioTokens:        usage.CompletionTokensDetails.AudioTokens,
	}
}

func chatUsagePresent(usage openai.CompletionUsage) bool {
	return usage.JSON.PromptTokens.Valid() || usage.JSON.CompletionTokens.Valid()
}

// chatStream adapts the chat SSE stream to the shared provider stream
// contract. It assigns canonical part indices as deltas arrive and holds
// the finish event until the stream ends so usage rides along.
type chatStream struct {
	stream *ssestream.Stream[openai.ChatCompletionChunk]

	pending []streamRaw

	textPart  int
	toolParts map[int64]int
	nextPart  int

	finish    inference.FinishReason
	finishErr error
	usage     *rawUsage
	sawTools  bool
	ended     bool
	id        string
}

// transportChatGenerateStream opens the streaming chat request.
func transportChatGenerateStream(
	client openai.Client,
) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[streamRaw], error) {
		stream := client.Chat.Completions.NewStreaming(
			ctx,
			wireToChatParams(wire),
			requestMetadataOptions(wire)...,
		)
		if stream == nil {
			return nil, errdefs.NotAvailablef(
				"openai: nil chat stream handle (provider misbehaviour)")
		}
		if err := stream.Err(); err != nil {
			classified := classifyError(err)
			logInferenceStream(ctx, "generate", wire.model, classified, "")
			return nil, classified
		}
		logInferenceStream(ctx, "generate", wire.model, nil, "")
		return &chatStream{
			stream:    stream,
			textPart:  -1,
			toolParts: make(map[int64]int),
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
				classified := classifyError(err)
				logInferenceStream(ctx, "generate", "", classified, "")
				return streamRaw{}, classified
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

func (s *chatStream) apply(chunk openai.ChatCompletionChunk) {
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chatUsagePresent(chunk.Usage) {
		usage := chatUsageToRaw(chunk.Usage)
		s.usage = &usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	delta := choice.Delta
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
		finish, err := chatFinishReason(choice.FinishReason)
		if err != nil {
			s.finishErr = err
		} else {
			s.finish = finish
		}
	}
}

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

func (s *chatStream) textIndex() int {
	if s.textPart < 0 {
		s.textPart = s.assignPart()
	}
	return s.textPart
}

// decodeChatGenerateStream reuses the shared stream decoder: chat stream
// events already carry canonical part indices.
func decodeChatGenerateStream(
	ctx context.Context,
	raw streamRaw,
) (inference.GenerateStreamEvent, error) {
	return decodeGenerateStream(ctx, raw)
}
