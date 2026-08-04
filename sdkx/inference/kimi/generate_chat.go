package kimi

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// generateRaw is the transport stage's normalized completion: everything
// the decoder needs, nothing it does not.
type generateRaw struct {
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

// completionEnvelope is the chat completion response, including the
// Kimi-owned reasoning_content and cached_tokens extras.
type completionEnvelope struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
}

// usageWire is Kimi's usage object; cached_tokens rides top-level.
type usageWire struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

func (u *usageWire) toRaw() rawUsage {
	if u == nil {
		return rawUsage{}
	}
	return rawUsage{
		input:     u.PromptTokens,
		output:    u.CompletionTokens,
		total:     u.TotalTokens,
		cached:    u.CachedTokens,
		reasoning: u.ReasoningTokens,
		present:   u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0,
	}
}

// transportGenerate executes the compiled unary request.
func transportGenerate(client *kimiClient) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		wire.Stream = false
		var envelope completionEnvelope
		if err := client.postJSON(ctx, wire, &envelope); err != nil {
			return generateRaw{}, err
		}
		return completionToRaw(&envelope)
	}
}

func completionToRaw(envelope *completionEnvelope) (generateRaw, error) {
	if envelope == nil || len(envelope.Choices) == 0 {
		return generateRaw{}, errdefs.NotAvailablef("kimi: response carries no choices")
	}
	choice := envelope.Choices[0]
	finish, err := mapFinishReason(choice.FinishReason)
	if err != nil {
		return generateRaw{}, err
	}

	raw := generateRaw{
		reasoning: choice.Message.ReasoningContent,
		text:      choice.Message.Content,
		finish:    finish,
		usage:     envelope.Usage.toRaw(),
	}
	for _, call := range choice.Message.ToolCalls {
		raw.toolCalls = append(raw.toolCalls, rawToolCall{
			id:   call.ID,
			name: call.Function.Name,
			args: call.Function.Arguments,
		})
	}
	return raw, nil
}

// mapFinishReason translates the provider's terminal states.
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
	default:
		return inference.FinishOther, nil
	}
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
		usage.Input.CacheReadTokens = ptrInt64(raw.cached)
	}
	if raw.reasoning > 0 {
		usage.Output.ReasoningTokens = ptrInt64(raw.reasoning)
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	return usage
}
