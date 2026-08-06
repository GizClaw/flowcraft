package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// ---------------------------------------------------------------------------
// Wire → Responses API params. This conversion is total and pure: every
// field the compiler set has exactly one param destination.
// ---------------------------------------------------------------------------

func wireToParams(wire generateWire) responses.ResponseNewParams {
	items := make(responses.ResponseInputParam, 0, len(wire.items))
	for _, item := range wire.items {
		switch item.kind {
		case wireItemMessage:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRole(item.role),
					Content: messageContent(item.content),
				},
			})
		case wireItemToolCall:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID:    item.callID,
					Name:      item.name,
					Arguments: string(item.args),
				},
			})
		case wireItemToolResult:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: item.callID,
					Output: item.output,
				},
			})
		case wireItemReasoning:
			reasoning := responses.ResponseReasoningItemParam{
				ID:               item.reasoningID,
				EncryptedContent: param.NewOpt(item.encrypted),
			}
			if item.summary != "" {
				reasoning.Summary = []responses.ResponseReasoningItemSummaryParam{
					{Text: item.summary},
				}
			}
			items = append(items, responses.ResponseInputItemUnionParam{
				OfReasoning: &reasoning,
			})
		}
	}

	params := responses.ResponseNewParams{
		Model: wire.model,
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
		// Reasoning traces are worthless to consumers without their
		// encrypted payload: without it the reasoning cannot round-trip
		// into later context, which breaks agent loops silently. The
		// include flag costs nothing on non-reasoning models.
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		},
	}
	if wire.maxTokens != nil {
		params.MaxOutputTokens = param.NewOpt(*wire.maxTokens)
	}
	if wire.temperature != nil {
		params.Temperature = param.NewOpt(*wire.temperature)
	}
	if wire.topP != nil {
		params.TopP = param.NewOpt(*wire.topP)
	}
	if wire.reasoning != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(wire.reasoning),
		}
	}
	if wire.textFormat != nil {
		params.Text = textFormatParam(wire.textFormat)
	}
	for _, definition := range wire.tools {
		toolParam := responses.FunctionToolParam{
			Name:       definition.name,
			Parameters: schemaMap(definition.schema),
		}
		if definition.description != "" {
			toolParam.Description = param.NewOpt(definition.description)
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{
			OfFunction: &toolParam,
		})
	}
	if wire.toolChoice != nil {
		params.ToolChoice = toolChoiceParam(wire.toolChoice)
	}
	return params
}

func messageContent(
	content []wireContent,
) responses.EasyInputMessageContentUnionParam {
	list := make(responses.ResponseInputMessageContentListParam, 0, len(content))
	for _, part := range content {
		switch part.kind {
		case wireContentText:
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: part.text},
			})
		case wireContentImage:
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: param.NewOpt(part.uri),
				},
			})
		}
	}
	return responses.EasyInputMessageContentUnionParam{OfInputItemContentList: list}
}

func textFormatParam(format *wireTextFormat) responses.ResponseTextConfigParam {
	switch format.kind {
	case "json_object":
		return responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
			},
		}
	case "json_schema":
		return responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigParamOfJSONSchema(
				format.name,
				schemaMap(format.schema),
			),
		}
	}
	return responses.ResponseTextConfigParam{}
}

func toolChoiceParam(choice *wireToolChoice) responses.ResponseNewParamsToolChoiceUnion {
	switch choice.mode {
	case "none":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsNone),
		}
	case "required":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired),
		}
	case "named":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice.name},
		}
	default:
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto),
		}
	}
}

// ---------------------------------------------------------------------------
// Unary transport and decode.
// ---------------------------------------------------------------------------

func transportGenerate(
	client openai.Client,
) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		response, err := client.Responses.New(ctx, wireToParams(wire))
		if err != nil {
			return generateRaw{}, classifyError(err)
		}
		return responseToRaw(response)
	}
}

// responseToRaw converts the SDK response into the provider-owned raw model,
// rejecting provider failures with classified errors.
func responseToRaw(response *responses.Response) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, fmt.Errorf("openai: empty responses object")
	}
	if response.Status == responses.ResponseStatusFailed {
		return generateRaw{}, classifyResponseError(
			string(response.Error.Code),
			response.Error.Message,
		)
	}
	raw := generateRaw{id: response.ID}
	for _, item := range response.Output {
		switch item.Type {
		case "reasoning":
			raw.reasonings = append(raw.reasonings, rawReasoning{
				id:        item.ID,
				text:      reasoningSummary(item.Summary),
				signature: item.EncryptedContent,
			})
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" {
					raw.texts = append(raw.texts, content.Text)
				}
			}
		case "function_call":
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   item.CallID,
				name: item.Name,
				args: []byte(item.Arguments),
			})
		}
	}
	raw.usage = responseUsage(response.Usage)
	raw.finish = responseFinish(response, len(raw.toolCalls) > 0)
	return raw, nil
}

func responseUsage(usage responses.ResponseUsage) rawUsage {
	raw := rawUsage{
		inputTokens:     usage.InputTokens,
		outputTokens:    usage.OutputTokens,
		totalTokens:     usage.TotalTokens,
		cachedTokens:    usage.InputTokensDetails.CachedTokens,
		reasoningTokens: usage.OutputTokensDetails.ReasoningTokens,
	}
	if raw.totalTokens == 0 {
		raw.totalTokens = raw.inputTokens + raw.outputTokens
	}
	return raw
}

func responseFinish(
	response *responses.Response,
	hasToolCalls bool,
) inference.FinishReason {
	if hasToolCalls {
		return inference.FinishToolCalls
	}
	if response.Status == responses.ResponseStatusIncomplete {
		return incompleteFinish(response.IncompleteDetails.Reason)
	}
	return inference.FinishCompleted
}

func incompleteFinish(reason string) inference.FinishReason {
	switch reason {
	case "max_output_tokens", "max_tokens":
		return inference.FinishMaxOutput
	case "content_filter":
		return inference.FinishContentFilter
	}
	return inference.FinishCompleted
}

func classifyResponseError(code, message string) error {
	err := fmt.Errorf("openai: response failed: %s %s", code, message)
	switch lower := strings.ToLower(code + " " + message); {
	case strings.Contains(lower, "rate"):
		return errdefs.RateLimit(err)
	case strings.Contains(lower, "auth"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "permission"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "filter"),
		strings.Contains(lower, "invalid"),
		strings.Contains(lower, "notfound"):
		return errdefs.Validation(err)
	default:
		return errdefs.NotAvailable(err)
	}
}

// reasoningSummary joins one reasoning item's summary entries. The
// canonical part is item-granular (matching where the encrypted payload
// lives), so the visible summary text joins with a blank line.
func reasoningSummary(summary []responses.ResponseReasoningItemSummary) string {
	texts := make([]string, 0, len(summary))
	for _, entry := range summary {
		if entry.Text != "" {
			texts = append(texts, entry.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func decodeGenerate(
	_ context.Context,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0,
		len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	// The API emits reasoning items before message and call items; the
	// canonical message keeps that order so context round-trips stay valid.
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{
			Text:      reasoning.text,
			Signature: reasoning.signature,
			ID:        reasoning.id,
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
		parts = append(parts, message.ToolCallPart{Call: message.Call{
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
	}, nil
}

func rawUsageCanonical(raw rawUsage) inference.Usage {
	usage := inference.Usage{
		InputTokens:  raw.inputTokens,
		OutputTokens: raw.outputTokens,
		TotalTokens:  raw.totalTokens,
	}
	if raw.cachedTokens > 0 {
		cached := raw.cachedTokens
		usage.Input.CacheReadTokens = &cached
	}
	if raw.reasoningTokens > 0 {
		reasoning := raw.reasoningTokens
		usage.Output.ReasoningTokens = &reasoning
		// The Responses API reports output_tokens inclusive of reasoning.
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	return usage
}
