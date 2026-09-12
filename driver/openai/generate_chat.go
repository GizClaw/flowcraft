package openai

// Chat Completions surface: the sink that spells the compiler's decisions as
// Chat Completions params, plus both transports and the response decoder.
// Chat Completions carries function calls inside the assistant message rather
// than as items of their own, which is where its item model differs from
// Responses.

import (
	"context"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"
)

// compileChat lowers a canonical request into Chat Completions params.
func compileChat(
	modelName string,
	entry catalogEntry,
) inference.GenerateCompiler[*chatRequest] {
	return compileGenerate(
		entry,
		func(shape inference.GenerateExecutionShape) *chatRequest {
			return newChatRequest(modelName, entry, shape)
		},
	)
}

// chatRequest is one compiled Chat Completions call: the SDK params the
// transports post, plus the per-request options for the fields the API leaves
// untyped (an aliased metadata envelope).
type chatRequest struct {
	params  openai.ChatCompletionNewParams
	options []option.RequestOption
	// assistant buffers the assistant turn a run of tool calls attaches to:
	// Chat Completions carries calls inside the message, so the compiler's
	// item order is reassembled here.
	assistant *openai.ChatCompletionAssistantMessageParam
}

// newChatRequest seeds the provider-wide policy of one Chat Completions call.
func newChatRequest(
	modelName string,
	entry catalogEntry,
	shape inference.GenerateExecutionShape,
) *chatRequest {
	request := &chatRequest{
		params: openai.ChatCompletionNewParams{Model: modelName},
	}
	// The driver states the retention decision instead of leaving it to a
	// provider default, exactly as the Responses surface does.
	request.params.Store = param.NewOpt(entry.dialect.store)
	if shape != inference.GenerateExecutionStream {
		return request
	}
	// stream_options carries the streaming policy. Both fields are opt-out,
	// and the object only goes out when a policy actually asks for a field:
	// some compatible endpoints reject it outright.
	options := openai.ChatCompletionStreamOptionsParam{}
	set := false
	if entry.dialect.chatStreamUsage() {
		options.IncludeUsage = openai.Bool(true)
		set = true
	}
	if obfuscation := entry.dialect.chatObfuscation(); obfuscation != nil {
		options.IncludeObfuscation = openai.Bool(*obfuscation)
		set = true
	}
	if set {
		request.params.StreamOptions = options
	}
	return request
}

// ---------------------------------------------------------------------------
// Sink
// ---------------------------------------------------------------------------

func (r *chatRequest) message(role string, content []contentPart) {
	r.flushAssistant()
	switch role {
	case string(message.RoleAssistant):
		assistant := &openai.ChatCompletionAssistantMessageParam{}
		if text := joinedText(content); text != "" {
			assistant.Content.OfString = openai.String(text)
		}
		r.assistant = assistant
	case string(message.RoleSystem):
		r.params.Messages = append(r.params.Messages,
			openai.SystemMessage(joinedText(content)))
	default: // user
		parts := chatUserContent(content)
		if len(parts) == 1 && parts[0].OfText != nil {
			r.params.Messages = append(r.params.Messages,
				openai.UserMessage(parts[0].OfText.Text))
			return
		}
		r.params.Messages = append(r.params.Messages,
			openai.UserMessage(parts))
	}
}

func (r *chatRequest) toolCall(callID, name string, args []byte) {
	if r.assistant == nil {
		r.assistant = &openai.ChatCompletionAssistantMessageParam{}
	}
	r.assistant.ToolCalls = append(r.assistant.ToolCalls,
		openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: callID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      name,
					Arguments: string(args),
				},
			},
		})
}

func (r *chatRequest) toolResult(callID string, content []contentPart) {
	r.flushAssistant()
	r.params.Messages = append(r.params.Messages,
		openai.ToolMessage(joinedText(content), callID))
}

// reasoning is unreachable on this surface: the compiler reports the drop for
// every reasoning item it sees in chat mode, because Chat Completions has no
// standardized reasoning round-trip to spell.
func (*chatRequest) reasoning(message.ReasoningPart, bool) {}

func (r *chatRequest) setTextFormat(format *inference.ResponseFormat) {
	switch format.Kind {
	case inference.ResponseJSONObject:
		r.params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	case inference.ResponseJSONSchema:
		r.params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   format.Name,
					Strict: param.NewOpt(true),
					Schema: schemaMap(format.Schema),
				},
			},
		}
	}
}

func (r *chatRequest) setMaxOutputTokens(tokens int64) {
	r.params.MaxCompletionTokens = param.NewOpt(tokens)
}

func (r *chatRequest) setTemperature(value float64) {
	r.params.Temperature = param.NewOpt(value)
}

func (r *chatRequest) setTopP(value float64) {
	r.params.TopP = param.NewOpt(value)
}

func (r *chatRequest) addTool(definition message.ToolDefinition) {
	r.params.Tools = append(r.params.Tools, openai.ChatCompletionToolUnionParam{
		OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        definition.Name,
				Description: openai.String(definition.Description),
				Parameters:  openai.FunctionParameters(schemaMap(definition.InputSchema)),
			},
		},
	})
}

func (r *chatRequest) setToolChoice(choice inference.ToolChoice) {
	switch choice.Kind {
	case inference.ToolChoiceNone, inference.ToolChoiceRequired:
		r.params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String(string(choice.Kind)),
		}
	case inference.ToolChoiceNamed:
		r.params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{
					Name: choice.Name,
				},
			},
		}
	case inference.ToolChoiceAuto:
		r.params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String("auto"),
		}
	}
}

func (r *chatRequest) setReasoningEffort(effort string) {
	r.params.ReasoningEffort = shared.ReasoningEffort(effort)
}

func (r *chatRequest) setVerbosity(level string) {
	r.params.Verbosity = openai.ChatCompletionNewParamsVerbosity(level)
}

func (r *chatRequest) setServiceTier(tier string) {
	r.params.ServiceTier = openai.ChatCompletionNewParamsServiceTier(tier)
}

func (r *chatRequest) setParallelToolCalls(value bool) {
	r.params.ParallelToolCalls = param.NewOpt(value)
}

// setMaxToolCalls is unreachable on this surface: Chat Completions has no
// tool-call budget, so the compiler rejects the knob before the sink is asked.
func (*chatRequest) setMaxToolCalls(int) {}

func (r *chatRequest) setSafetyIdentifier(identifier string) {
	r.params.SafetyIdentifier = param.NewOpt(identifier)
}

func (r *chatRequest) setPromptCacheKey(key string) {
	r.params.PromptCacheKey = param.NewOpt(key)
}

func (r *chatRequest) setRequestMetadata(
	envelope string,
	metadata map[string]string,
) {
	if envelope == "metadata" {
		r.params.Metadata = metadata
		return
	}
	// The SDK types only the native metadata object; a gateway envelope names
	// a field it cannot express, so it rides the raw-JSON option path.
	r.options = append(r.options, option.WithJSONSet(envelope, metadata))
}

// addHostedWebSearch is unreachable on this surface: Chat Completions has no
// hosted web_search tool, so the compiler rejects it before the sink is asked.
func (*chatRequest) addHostedWebSearch(*GenerateWebSearch, bool) {}

// flushAssistant appends the buffered assistant turn, if any. Tool calls
// attach to that turn, so it is only complete once the next role arrives.
func (r *chatRequest) flushAssistant() {
	if r.assistant == nil {
		return
	}
	r.params.Messages = append(r.params.Messages,
		openai.ChatCompletionMessageParamUnion{OfAssistant: r.assistant})
	r.assistant = nil
}

// joinedText concatenates a carried content run into the plain string Chat
// Completions carries. The compiler lowered structured data to text and
// replaced every part this surface cannot hold with a text placeholder, so
// nothing is lost here.
func joinedText(content []contentPart) string {
	var builder strings.Builder
	for _, part := range content {
		if part.kind == contentText {
			builder.WriteString(part.text)
		}
	}
	return builder.String()
}

func chatUserContent(content []contentPart) []openai.ChatCompletionContentPartUnionParam {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(content))
	for _, part := range content {
		if part.kind == contentImage {
			parts = append(parts, openai.ChatCompletionContentPartUnionParam{
				OfImageURL: &openai.ChatCompletionContentPartImageParam{
					ImageURL: openai.ChatCompletionContentPartImageImageURLParam{
						URL: part.uri,
					},
				},
			})
			continue
		}
		parts = append(parts, openai.ChatCompletionContentPartUnionParam{
			OfText: &openai.ChatCompletionContentPartTextParam{Text: part.text},
		})
	}
	return parts
}

// ---------------------------------------------------------------------------
// Transport and decode.
// ---------------------------------------------------------------------------

// transportChatGenerate executes the compiled request against the chat
// completions endpoint.
func transportChatGenerate(
	client openai.Client,
) inference.Transport[*chatRequest, generateRaw] {
	return func(ctx context.Context, request *chatRequest) (generateRaw, error) {
		modelName := string(request.params.Model)
		response, err := client.Chat.Completions.New(
			ctx,
			request.params,
			request.options...,
		)
		if err != nil {
			classified := classifyError(err)
			inference.LogProviderCall(ctx, providerID, "generate", modelName, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := chatCompletionToRaw(response)
		if err != nil {
			inference.LogProviderCall(ctx, providerID, "generate", modelName, err, "", "")
			return generateRaw{}, err
		}
		inference.LogProviderCall(ctx, providerID, "generate", modelName, nil, "", raw.id)
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

	// requestID is the provider x-request-id header captured at stream
	// open; it survives truncation where the terminal event does not.
	requestID string

	// model identifies the requested model for stream lifecycle warnings.
	model string
}

// transportChatGenerateStream opens the streaming chat request.
func transportChatGenerateStream(
	client openai.Client,
) inference.Transport[*chatRequest, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		request *chatRequest,
	) (inference.ProviderStream[streamRaw], error) {
		modelName := string(request.params.Model)
		var requestID string
		opts := append([]option.RequestOption(nil), request.options...)
		opts = append(opts, captureRequestID(&requestID))
		stream := client.Chat.Completions.NewStreaming(
			ctx,
			request.params,
			opts...,
		)
		if stream == nil {
			return nil, errdefs.NotAvailablef(
				"openai: nil chat stream handle (provider misbehaviour)")
		}
		if err := stream.Err(); err != nil {
			classified := classifyError(err)
			inference.LogProviderStream(ctx, providerID, "generate", modelName, classified, "")
			return nil, classified
		}
		inference.LogProviderStream(ctx, providerID, "generate", modelName, nil, "")
		return &chatStream{
			stream:    stream,
			textPart:  -1,
			toolParts: make(map[int64]int),
			requestID: requestID,
			model:     modelName,
		}, nil
	}
}

func (s *chatStream) RequestID() string  { return s.requestID }
func (s *chatStream) ResponseID() string { return s.id }

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
				inference.LogProviderStream(ctx, providerID, "generate", "", classified, "")
				return streamRaw{}, classified
			}
			if synthesized := s.end(); synthesized != "" {
				logChatFinishSynthesized(
					ctx, s.model, s.requestID, s.id, synthesized)
			}
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

// end emits the terminal event exactly once: the recorded finish reason
// (defaulting to completed, or tool_calls when calls streamed without an
// explicit reason) plus the usage chunk's accounting. It returns the
// synthesized reason when the provider never sent an explicit one, so
// callers can warn that the stream may have been truncated.
func (s *chatStream) end() string {
	if s.ended {
		return ""
	}
	s.ended = true
	if s.finishErr != nil {
		return ""
	}
	synthesized := ""
	finish := s.finish
	if finish == "" && s.sawTools {
		finish = inference.FinishToolCalls
		synthesized = string(finish)
	}
	if finish == "" {
		finish = inference.FinishCompleted
		synthesized = string(finish)
	}
	s.pending = append(s.pending, streamRaw{
		kind:        streamRawFinish,
		finish:      finish,
		usage:       s.usage,
		responseID:  s.id,
		synthesized: synthesized != "",
	})
	return synthesized
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
