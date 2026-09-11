package anthropic

// Anthropic SDK bindings: transport, response → raw, decode. The compiler
// lowers canonical requests straight into SDK params; this file speaks the
// SDK on the way back and owns the pieces of the param vocabulary the
// compiler shares (tool schemas, tool choice, argument decoding).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// argsValue decodes tool-call arguments for the SDK's any-typed input. An
// empty or malformed payload degrades to an empty object; validity is the
// compiler's contract on the way in.
func argsValue(raw []byte) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{}
	}
	return value
}

// toolInputSchema lowers a canonical JSON schema into the SDK's typed
// fields: properties and required lift out, the rest rides ExtraFields.
func toolInputSchema(raw []byte) anthropicgo.ToolInputSchemaParam {
	decoded := schemaMap(raw)
	param := anthropicgo.ToolInputSchemaParam{
		Properties:  decoded["properties"],
		ExtraFields: map[string]any{},
	}
	if required, ok := decoded["required"].([]any); ok {
		for _, item := range required {
			if name, ok := item.(string); ok {
				param.Required = append(param.Required, name)
			}
		}
	}
	for key, value := range decoded {
		switch key {
		case "type", "properties", "required":
		default:
			param.ExtraFields[key] = value
		}
	}
	if len(param.ExtraFields) == 0 {
		param.ExtraFields = nil
	}
	return param
}

func toolChoiceParam(choice inference.ToolChoice) anthropicgo.ToolChoiceUnionParam {
	switch choice.Kind {
	case inference.ToolChoiceNone:
		return anthropicgo.ToolChoiceUnionParam{
			OfNone: &anthropicgo.ToolChoiceNoneParam{},
		}
	case inference.ToolChoiceRequired:
		return anthropicgo.ToolChoiceUnionParam{
			OfAny: &anthropicgo.ToolChoiceAnyParam{},
		}
	case inference.ToolChoiceNamed:
		return anthropicgo.ToolChoiceUnionParam{
			OfTool: &anthropicgo.ToolChoiceToolParam{Name: choice.Name},
		}
	default:
		return anthropicgo.ToolChoiceUnionParam{
			OfAuto: &anthropicgo.ToolChoiceAutoParam{},
		}
	}
}

// ---------------------------------------------------------------------------
// Transport + response lowering.
// ---------------------------------------------------------------------------

func transportGenerate(
	client anthropicgo.Client,
) inference.Transport[anthropicgo.MessageNewParams, generateRaw] {
	return func(ctx context.Context, params anthropicgo.MessageNewParams) (generateRaw, error) {
		modelName := string(params.Model)
		message, err := client.Messages.New(ctx, params)
		if err != nil {
			classified := classifyError(err)
			logInferenceCall(ctx, "generate", modelName, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := messageToRaw(message)
		if err != nil {
			logInferenceCall(ctx, "generate", modelName, err, "", "")
			return generateRaw{}, err
		}
		logInferenceCall(ctx, "generate", modelName, nil, "", raw.id)
		return raw, nil
	}
}

// messageToRaw lowers the SDK message into the provider-owned raw model.
// Thinking blocks lower with their signature so the reasoning trace can
// round-trip through later context; redacted blocks carry only the opaque
// data, which the canonical reasoning part keeps in the signature slot.
func messageToRaw(message *anthropicgo.Message) (generateRaw, error) {
	if message == nil {
		return generateRaw{}, fmt.Errorf("anthropic: empty message object")
	}
	raw := generateRaw{id: message.ID}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			raw.texts = append(raw.texts, block.Text)
		case "thinking":
			raw.reasonings = append(raw.reasonings, rawReasoning{
				text:      block.Thinking,
				signature: block.Signature,
			})
		case "redacted_thinking":
			raw.reasonings = append(raw.reasonings, rawReasoning{
				signature: block.Data,
			})
		case "tool_use":
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   block.ID,
				name: block.Name,
				args: []byte(block.Input),
			})
		}
	}
	raw.usage = usageFromSDK(message.Usage)
	raw.finish = stopReasonFinish(message.StopReason, len(raw.toolCalls) > 0)
	return raw, nil
}

// usageFromSDK lowers the SDK usage object onto the provider raw model,
// keeping every counter the Messages API reports: cache totals and their
// per-TTL breakdown, thinking tokens, and server tool request counts.
func usageFromSDK(usage anthropicgo.Usage) rawUsage {
	return rawUsage{
		inputTokens:       usage.InputTokens,
		outputTokens:      usage.OutputTokens,
		cacheReadTokens:   usage.CacheReadInputTokens,
		cacheWriteTokens:  usage.CacheCreationInputTokens,
		cacheWrite5m:      usage.CacheCreation.Ephemeral5mInputTokens,
		cacheWrite1h:      usage.CacheCreation.Ephemeral1hInputTokens,
		thinkingTokens:    usage.OutputTokensDetails.ThinkingTokens,
		webSearchRequests: usage.ServerToolUse.WebSearchRequests,
		webFetchRequests:  usage.ServerToolUse.WebFetchRequests,
	}
}

// stopReasonFinish maps the API's stop reasons onto canonical finish
// reasons. tool_use wins when calls exist; pause_turn completes the turn as
// the API hands control back without an error.
func stopReasonFinish(
	reason anthropicgo.StopReason,
	hasToolCalls bool,
) inference.FinishReason {
	switch reason {
	case anthropicgo.StopReasonMaxTokens,
		anthropicgo.StopReasonModelContextWindowExceeded:
		return inference.FinishMaxOutput
	case anthropicgo.StopReasonToolUse:
		return inference.FinishToolCalls
	case anthropicgo.StopReasonRefusal:
		return inference.FinishRefusal
	default:
		if hasToolCalls {
			return inference.FinishToolCalls
		}
		return inference.FinishCompleted
	}
}

// ---------------------------------------------------------------------------
// Decode — raw → canonical response. Pure.
// ---------------------------------------------------------------------------

func decodeGenerate(
	_ context.Context,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0,
		len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	// The API orders thinking blocks first in its responses; the canonical
	// message keeps that order so context round-trips stay valid.
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{
			Text:      reasoning.text,
			Signature: reasoning.signature,
		})
	}
	for _, text := range raw.texts {
		parts = append(parts, message.TextPart{Text: text})
	}
	for _, call := range raw.toolCalls {
		arguments := json.RawMessage(call.args)
		if !json.Valid(arguments) || len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
			ID:        call.id,
			Name:      call.name,
			Arguments: arguments,
		}})
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Usage:        rawUsageCanonical(raw.usage),
		Metadata:     inference.Metadata{ResponseID: raw.id},
	}, nil
}

func rawUsageCanonical(raw rawUsage) inference.Usage {
	usage := inference.Usage{
		InputTokens:  raw.inputTokens,
		OutputTokens: raw.outputTokens,
		TotalTokens:  raw.inputTokens + raw.outputTokens,
	}
	if raw.cacheReadTokens > 0 {
		read := raw.cacheReadTokens
		usage.Input.CacheReadTokens = &read
	}
	if raw.cacheWriteTokens > 0 {
		write := raw.cacheWriteTokens
		usage.Input.CacheWriteTokens = &write
	}
	// Preserve the per-TTL cache-write split only when it agrees with the
	// total; a provider that reports the total without the split stays
	// valid because the breakdown is optional.
	cacheWrites := make([]inference.CacheWriteUsage, 0, 2)
	if raw.cacheWrite5m > 0 {
		cacheWrites = append(cacheWrites, inference.CacheWriteUsage{
			TTL:    inference.CacheTTL5Minutes,
			Tokens: raw.cacheWrite5m,
		})
	}
	if raw.cacheWrite1h > 0 {
		cacheWrites = append(cacheWrites, inference.CacheWriteUsage{
			TTL:    inference.CacheTTL1Hour,
			Tokens: raw.cacheWrite1h,
		})
	}
	if len(cacheWrites) > 0 &&
		raw.cacheWrite5m+raw.cacheWrite1h == raw.cacheWriteTokens {
		usage.Input.CacheWrites = cacheWrites
	}
	if raw.thinkingTokens > 0 {
		thinking := raw.thinkingTokens
		usage.Output.ReasoningTokens = &thinking
		// thinking_tokens is always a subset of output_tokens.
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	if raw.webSearchRequests > 0 {
		usage.Tools = append(usage.Tools, inference.ToolUsage{
			Kind:     inference.ToolUsageWebSearch,
			Requests: raw.webSearchRequests,
		})
	}
	if raw.webFetchRequests > 0 {
		usage.Tools = append(usage.Tools, inference.ToolUsage{
			Kind:     inference.ToolUsageWebFetch,
			Requests: raw.webFetchRequests,
		})
	}
	return usage
}
