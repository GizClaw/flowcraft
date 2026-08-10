package deepseek

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
)

// wireToParams converts the wire model into openai-go chat completion
// params plus the per-request JSON overrides DeepSeek owns (thinking,
// reasoning_effort): the SDK does not type them, so they ride
// option.WithJSONSet.
func wireToParams(wire generateWire) (openaigo.ChatCompletionNewParams, []option.RequestOption) {
	params := openaigo.ChatCompletionNewParams{
		Model:    wire.model,
		Messages: wireMessagesToParams(wire.messages),
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
					Function: openaigo.ChatCompletionNamedToolChoiceFunctionParam{Name: choice.name},
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
		overrides = append(overrides, option.WithJSONSet("thinking", map[string]any{"type": kind}))
	}
	if wire.effort != "" {
		overrides = append(overrides, option.WithJSONSet("reasoning_effort", wire.effort))
	}
	return params, overrides
}

func wireMessagesToParams(messages []wireMessage) []openaigo.ChatCompletionMessageParamUnion {
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
				assistant.ToolCalls = append(assistant.ToolCalls, openaigo.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openaigo.ChatCompletionMessageFunctionToolCallParam{
						ID: call.id,
						Function: openaigo.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.name,
							Arguments: string(call.args),
						},
					},
				})
			}
			// DeepSeek 400s a thinking-mode request whose tool-calling
			// assistant turns lack reasoning_content, so a turn with tool
			// calls but no trace still carries the field — empty, which
			// the API accepts.
			if message.hasReasoning {
				assistant.SetExtraFields(map[string]any{"reasoning_content": message.reasoning})
			} else if len(message.toolCalls) > 0 {
				assistant.SetExtraFields(map[string]any{"reasoning_content": ""})
			}
			out = append(out, openaigo.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		default:
			out = append(out, openaigo.UserMessage(message.text))
		}
	}
	return out
}

// transportGenerate executes the compiled request against the chat
// completions endpoint.
func transportGenerate(client openaigo.Client) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		params, overrides := wireToParams(wire)
		response, err := client.Chat.Completions.New(ctx, params, overrides...)
		if err != nil {
			return generateRaw{}, classifyError(err)
		}
		return completionToRaw(response)
	}
}

func completionToRaw(response *openaigo.ChatCompletion) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, errdefs.NotAvailablef("deepseek: nil response with no error (provider misbehaviour)")
	}
	if len(response.Choices) == 0 {
		return generateRaw{}, errdefs.NotAvailablef("deepseek: response carries no choices")
	}
	choice := response.Choices[0]
	finish, err := mapFinishReason(choice.FinishReason)
	if err != nil {
		return generateRaw{}, err
	}

	raw := generateRaw{
		id:        response.ID,
		reasoning: reasoningContentOf(choice.Message.JSON.ExtraFields),
		text:      choice.Message.Content,
		finish:    finish,
		usage:     usageToRaw(response.Usage),
	}
	for _, call := range choice.Message.ToolCalls {
		function := call.AsFunction()
		raw.toolCalls = append(raw.toolCalls, rawToolCall{
			id:   function.ID,
			name: function.Function.Name,
			args: function.Function.Arguments,
		})
	}
	return raw, nil
}

// mapFinishReason translates the provider's terminal states. DeepSeek adds
// insufficient_system_resource — the request was interrupted on their side,
// so it classifies as a retryable provider failure rather than a finish.
func mapFinishReason(reason string) (inference.FinishReason, error) {
	switch reason {
	case "", "stop":
		return inference.FinishCompleted, nil
	case "length":
		return inference.FinishMaxOutput, nil
	case "tool_calls":
		return inference.FinishToolCalls, nil
	case "content_filter":
		return inference.FinishContentFilter, nil
	case "insufficient_system_resource":
		return "", errdefs.NotAvailablef("deepseek: request interrupted: insufficient system resource")
	default:
		return inference.FinishOther, nil
	}
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
	return raw
}

// decodeGenerate assembles the canonical response: reasoning trace first
// (it is the model's process, ahead of its answer), then text, then tool
// calls.
func decodeGenerate(_ context.Context, raw generateRaw) (inference.GenerateResponse, error) {
	var parts []message.Part
	if raw.reasoning != "" {
		parts = append(parts, message.ReasoningPart{Text: raw.reasoning})
	}
	if raw.text != "" {
		parts = append(parts, message.TextPart{Text: raw.text})
	}
	for _, call := range raw.toolCalls {
		arguments := json.RawMessage(call.args)
		if len(arguments) == 0 || !json.Valid(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		parts = append(parts, message.ToolCallPart{Call: message.Call{
			ID:        call.id,
			Name:      call.name,
			Arguments: arguments,
		}})
	}

	response := inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Metadata:     inference.Metadata{ResponseID: raw.id},
	}
	if raw.usage.present {
		response.Usage = rawUsageCanonical(raw.usage)
	}
	return response, nil
}

func rawUsageCanonical(raw rawUsage) inference.Usage {
	usage := inference.Usage{
		InputTokens:  raw.input,
		OutputTokens: raw.output,
		TotalTokens:  raw.total,
	}
	if raw.cached > 0 {
		usage.Input.CacheReadTokens = new(raw.cached)
	}
	if raw.reasoning > 0 {
		usage.Output.ReasoningTokens = new(raw.reasoning)
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	return usage
}
