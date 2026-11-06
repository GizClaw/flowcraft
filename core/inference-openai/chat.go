package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Which canonical content kinds each chat role can carry. The wire is stricter
// than the canonical model — a system turn is text-only, an assistant turn
// carries text and tool calls — so an unmapped kind is rejected rather than
// dropped.
var (
	chatSystemKinds    = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true}
	chatUserKinds      = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true, message.PartImage: true, message.PartAudio: true}
	chatAssistantKinds = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true, message.PartToolCall: true}
)

// chatCompile maps a canonical request onto Chat Completions parameters. It
// performs no I/O, so a conformance test can marshal the parameters and compare
// them against expected JSON.
func (d *driver) chatCompile(
	_ context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.ChatCompletionNewParams], error) {
	if err := d.preflight(request); err != nil {
		return protocol.Compiled[openai.ChatCompletionNewParams]{}, err
	}
	options, err := generationOptions(request.Extensions)
	if err != nil {
		return protocol.Compiled[openai.ChatCompletionNewParams]{}, err
	}
	params, err := chatParams(request, options, shape)
	if err != nil {
		return protocol.Compiled[openai.ChatCompletionNewParams]{}, err
	}
	fields := append(honoredFields(request), extensionFields(options)...)
	return protocol.Compiled[openai.ChatCompletionNewParams]{
		Wire:     params,
		Fields:   fields,
		Warnings: reasoningWarnings(request),
	}, nil
}

func chatParams(
	request inference.GenerateRequest,
	options *GenerationOptions,
	shape protocol.Shape,
) (openai.ChatCompletionNewParams, error) {
	messages, err := chatMessages(request)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	tools, err := chatTools(request.Tools.Definitions())
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(request.Model.ID.Name),
		Messages: messages,
		Tools:    tools,
	}
	if choice := request.Tools.Choice(); choice != nil {
		params.ToolChoice = chatToolChoice(*choice)
	}
	if err := chatApply(&params, options); err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	if shape == protocol.ShapeStream {
		// Without this the provider omits usage entirely on the streaming path,
		// and the assembled response would report zero tokens.
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
	}
	return params, nil
}

// chatMessages walks the canonical conversation into chat messages. A tool turn
// carrying several results becomes several messages, because the wire models one
// result per message.
func chatMessages(request inference.GenerateRequest) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(request.Context)+1)
	for i, item := range request.Context {
		mapped, err := chatMessage(item)
		if err != nil {
			return nil, fmt.Errorf("context message %d: %w", i, err)
		}
		out = append(out, mapped...)
	}
	mapped, err := chatMessage(request.Input.Message())
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	return append(out, mapped...), nil
}

func chatMessage(item message.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	switch item.Role {
	case message.RoleSystem:
		if err := rejectUnsupportedParts(item, chatSystemKinds); err != nil {
			return nil, err
		}
		return []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(textOf(item))}, nil

	case message.RoleUser:
		if err := rejectUnsupportedParts(item, chatUserKinds); err != nil {
			return nil, err
		}
		content, hasMedia, err := chatUserContent(item)
		if err != nil {
			return nil, err
		}
		if !hasMedia {
			return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(textOf(item))}, nil
		}
		return []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{
					OfArrayOfContentParts: content,
				},
			},
		}}, nil

	case message.RoleAssistant:
		if err := rejectUnsupportedParts(item, chatAssistantKinds); err != nil {
			return nil, err
		}
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if text := textOf(item); text != "" {
			assistant.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
				OfString: openai.String(text),
			}
		}
		for _, call := range item.ToolCalls() {
			assistant.ToolCalls = append(
				assistant.ToolCalls,
				openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.Name,
							Arguments: string(call.Arguments),
						},
					},
				},
			)
		}
		return []openai.ChatCompletionMessageParamUnion{{OfAssistant: &assistant}}, nil

	case message.RoleTool:
		var out []openai.ChatCompletionMessageParamUnion
		for _, part := range item.Content.Parts {
			result, ok := part.(message.ToolResultPart)
			if !ok {
				return nil, unsupportedPart(part.Kind())
			}
			out = append(out, openai.ToolMessage(result.Result.Content.Text(), result.Result.CallID))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("openai: tool message carries no tool result")
		}
		return out, nil

	default:
		return nil, fmt.Errorf("openai: role %q is not supported", item.Role)
	}
}

// chatUserContent builds the content array for a user turn that carries media.
// A text-only turn never reaches here: it uses the plain string form, which every
// OpenAI-compatible server accepts.
func chatUserContent(item message.Message) ([]openai.ChatCompletionContentPartUnionParam, bool, error) {
	var out []openai.ChatCompletionContentPartUnionParam
	hasMedia := false
	for _, part := range item.Content.Parts {
		switch typed := part.(type) {
		case message.TextPart:
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfText: &openai.ChatCompletionContentPartTextParam{Text: typed.Text},
			})
		case message.ImagePart:
			url, err := imageURL(typed.Source)
			if err != nil {
				return nil, false, err
			}
			hasMedia = true
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfImageURL: &openai.ChatCompletionContentPartImageParam{
					ImageURL: openai.ChatCompletionContentPartImageImageURLParam{URL: url},
				},
			})
		case message.AudioPart:
			data, format, err := audioPayload(typed.Source)
			if err != nil {
				return nil, false, err
			}
			hasMedia = true
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfInputAudio: &openai.ChatCompletionContentPartInputAudioParam{
					InputAudio: openai.ChatCompletionContentPartInputAudioInputAudioParam{
						Data:   data,
						Format: format,
					},
				},
			})
		}
	}
	return out, hasMedia, nil
}

func chatTools(definitions []message.ToolDefinition) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(definitions) == 0 {
		return nil, nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		parameters, err := schemaObject(definition)
		if err != nil {
			return nil, err
		}
		out = append(out, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        definition.Name,
					Description: openai.String(definition.Description),
					Parameters:  parameters,
				},
			},
		})
	}
	return out, nil
}

func chatToolChoice(choice inference.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
	if choice.Mode == inference.ToolChoiceFunction {
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: choice.Name},
			},
		}
	}
	return openai.ChatCompletionToolChoiceOptionUnionParam{
		OfAuto: openai.String(string(choice.Mode)),
	}
}

// chatCall performs the provider I/O. It is the only stage that touches the
// network, which is why a fake transport can test everything else.
func (d *driver) chatCall(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.ChatCompletionNewParams,
) (*openai.ChatCompletion, error) {
	extra, err := extensionJSONSet(request.Extensions)
	if err != nil {
		return nil, err
	}
	opts, err := bodyOptions(params, extra)
	if err != nil {
		return nil, err
	}
	auth, err := d.authOptions(request.Model.Profile)
	if err != nil {
		return nil, err
	}
	return d.client.Chat.Completions.New(ctx, params, append(opts, auth...)...)
}

// chatDecode maps the provider's answer onto the canonical response.
func (d *driver) chatDecode(
	_ context.Context,
	_ inference.GenerateRequest,
	raw *openai.ChatCompletion,
) (inference.GenerateResponse, error) {
	if raw == nil {
		return inference.GenerateResponse{}, fmt.Errorf("openai: nil chat completion")
	}
	if len(raw.Choices) == 0 {
		return inference.GenerateResponse{}, fmt.Errorf("openai: chat completion carried no choice")
	}
	choice := raw.Choices[0]

	parts := make([]message.Part, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != "" {
		parts = append(parts, message.TextPart{Text: choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: toolArguments(call.Function.Arguments),
		}})
	}
	finish, err := chatFinishReason(choice.FinishReason, len(choice.Message.ToolCalls))
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: finish,
		Usage:        chatUsage(raw),
		Metadata: inference.Metadata{
			Model:      model.ModelID{Provider: ProviderID, Name: string(raw.Model)},
			ResponseID: raw.ID,
		},
	}, nil
}

func chatFinishReason(reason string, toolCalls int) (inference.FinishReason, error) {
	switch reason {
	case "stop":
		if toolCalls > 0 {
			return inference.FinishToolCalls, nil
		}
		return inference.FinishStop, nil
	case "length":
		return inference.FinishLength, nil
	case "tool_calls", "function_call":
		return inference.FinishToolCalls, nil
	case "content_filter":
		return inference.FinishContentFilter, nil
	default:
		return "", inference.NewError(
			inference.ProviderResponseUnusable,
			"",
			fmt.Errorf("openai: unknown finish reason %q", reason),
		)
	}
}

func chatUsage(raw *openai.ChatCompletion) usage.Usage {
	out := usage.Usage{
		InputTokens:  raw.Usage.PromptTokens,
		OutputTokens: raw.Usage.CompletionTokens,
		TotalTokens:  raw.Usage.TotalTokens,
	}
	if cached := raw.Usage.PromptTokensDetails.CachedTokens; cached > 0 {
		out.Input.CacheReadTokens = &cached
	}
	return out
}

// schemaObject decodes a tool input schema into the untyped object the SDK
// expects. The canonical definition already validated it as a JSON object.
func schemaObject(definition message.ToolDefinition) (map[string]any, error) {
	var parameters map[string]any
	if err := json.Unmarshal(definition.InputSchema, &parameters); err != nil {
		return nil, fmt.Errorf("openai: tool %q schema: %w", definition.Name, err)
	}
	return parameters, nil
}

// toolArguments normalizes accumulated arguments into a JSON object: a call with
// no arguments means an empty object, not invalid JSON.
func toolArguments(arguments string) json.RawMessage {
	if arguments == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(arguments)
}

// chatApply maps per-call options onto chat parameters. Options the chat surface
// cannot express are rejected rather than ignored.
func chatApply(params *openai.ChatCompletionNewParams, options *GenerationOptions) error {
	if options == nil {
		return nil
	}
	if options.Temperature != nil {
		params.Temperature = openai.Float(*options.Temperature)
	}
	if options.TopP != nil {
		params.TopP = openai.Float(*options.TopP)
	}
	if options.MaxOutputTokens != nil {
		params.MaxCompletionTokens = openai.Int(int64(*options.MaxOutputTokens))
	}
	if options.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(options.ReasoningEffort)
	}
	if options.ResponseFormat != nil {
		format, err := chatResponseFormat(*options.ResponseFormat)
		if err != nil {
			return err
		}
		params.ResponseFormat = format
	}
	if options.ServiceTier != "" {
		params.ServiceTier = openai.ChatCompletionNewParamsServiceTier(options.ServiceTier)
	}
	if options.ParallelToolCalls != nil {
		params.ParallelToolCalls = openai.Bool(*options.ParallelToolCalls)
	}
	if options.SafetyIdentifier != "" {
		params.SafetyIdentifier = openai.String(options.SafetyIdentifier)
	}
	if options.PromptCacheKey != "" {
		params.PromptCacheKey = openai.String(options.PromptCacheKey)
	}
	switch {
	case options.MaxToolCalls != nil:
		return unsupportedOption("max_tool_calls", "responses")
	case options.Verbosity != "":
		return unsupportedOption("verbosity", "responses")
	case options.WebSearch != nil:
		return unsupportedOption("web_search", "responses")
	case options.Store != nil:
		return unsupportedOption("store", "responses")
	}
	return nil
}

// chatResponseFormat maps the canonical format onto the chat surface's union.
func chatResponseFormat(format ResponseFormat) (openai.ChatCompletionNewParamsResponseFormatUnion, error) {
	switch format.Kind {
	case FormatJSONObject:
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}, nil
	case FormatJSONSchema:
		var schema map[string]any
		if err := json.Unmarshal(format.Schema, &schema); err != nil {
			return openai.ChatCompletionNewParamsResponseFormatUnion{}, fmt.Errorf(
				"openai: response schema: %w", err,
			)
		}
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   format.Name,
					Schema: schema,
					Strict: openai.Bool(format.Strict),
				},
			},
		}, nil
	default:
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfText: &shared.ResponseFormatTextParam{},
		}, nil
	}
}
