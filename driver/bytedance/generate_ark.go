package bytedance

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func appendInputItem(ark *arkresponses.ResponsesRequest, item *arkresponses.InputItem) {
	if ark.Input == nil || ark.Input.GetListValue() == nil {
		ark.Input = &arkresponses.ResponsesInput{
			Union: &arkresponses.ResponsesInput_ListValue{
				ListValue: &arkresponses.InputItemList{},
			},
		}
	}
	list := ark.Input.GetListValue()
	list.ListValue = append(list.ListValue, item)
}

func arkMessageItem(role string, content []*arkresponses.ContentItem) *arkresponses.InputItem {
	return &arkresponses.InputItem{
		Union: &arkresponses.InputItem_EasyMessage{
			EasyMessage: &arkresponses.ItemEasyMessage{
				Type: arkresponses.ItemType_message.Enum(),
				Role: arkMessageRole(role),
				Content: &arkresponses.MessageContent{
					Union: &arkresponses.MessageContent_ListValue{
						ListValue: &arkresponses.ContentItemList{ListValue: content},
					},
				},
			},
		},
	}
}

func arkContentText(text string) *arkresponses.ContentItem {
	return &arkresponses.ContentItem{
		Union: &arkresponses.ContentItem_Text{
			Text: &arkresponses.ContentItemText{
				Type: arkresponses.ContentItemType_input_text,
				Text: text,
			},
		},
	}
}

func arkContentImage(uri string) *arkresponses.ContentItem {
	return &arkresponses.ContentItem{
		Union: &arkresponses.ContentItem_Image{
			Image: &arkresponses.ContentItemImage{
				Type:     arkresponses.ContentItemType_input_image,
				ImageUrl: &uri,
			},
		},
	}
}

func arkContentVideo(uri string) *arkresponses.ContentItem {
	return &arkresponses.ContentItem{
		Union: &arkresponses.ContentItem_Video{
			Video: &arkresponses.ContentItemVideo{
				Type:     arkresponses.ContentItemType_input_video,
				VideoUrl: uri,
			},
		},
	}
}

func arkContentAudio(uri string) *arkresponses.ContentItem {
	return &arkresponses.ContentItem{
		Union: &arkresponses.ContentItem_Audio{
			Audio: &arkresponses.ContentItemAudio{
				Type:     arkresponses.ContentItemType_input_audio,
				AudioUrl: uri,
			},
		},
	}
}

func arkToolCallItem(callID, name string, args []byte) *arkresponses.InputItem {
	return &arkresponses.InputItem{
		Union: &arkresponses.InputItem_FunctionToolCall{
			FunctionToolCall: &arkresponses.ItemFunctionToolCall{
				Type:      arkresponses.ItemType_function_call,
				CallId:    callID,
				Name:      name,
				Arguments: string(bytesClone(args)),
			},
		},
	}
}

func arkToolResultItem(callID, output string) *arkresponses.InputItem {
	return &arkresponses.InputItem{
		Union: &arkresponses.InputItem_FunctionToolCallOutput{
			FunctionToolCallOutput: &arkresponses.ItemFunctionToolCallOutput{
				Type:   arkresponses.ItemType_function_call_output,
				CallId: callID,
				Output: output,
			},
		},
	}
}

func arkTool(definition message.ToolDefinition) *arkresponses.ResponsesTool {
	tool := &arkresponses.ToolFunction{
		Type:       arkresponses.ToolType_function,
		Name:       definition.Name,
		Parameters: &arkresponses.Bytes{Value: bytesClone(definition.InputSchema)},
	}
	if definition.Description != "" {
		description := definition.Description
		tool.Description = &description
	}
	return &arkresponses.ResponsesTool{
		Union: &arkresponses.ResponsesTool_ToolFunction{ToolFunction: tool},
	}
}

func setArkThinking(ark *arkresponses.ResponsesRequest, enabled *bool, effort string) {
	switch {
	case enabled != nil && !*enabled:
		ark.Thinking = &arkresponses.ResponsesThinking{
			Type: arkresponses.ThinkingType_disabled.Enum(),
		}
	case effort != "" || (enabled != nil && *enabled):
		ark.Thinking = &arkresponses.ResponsesThinking{
			Type: arkresponses.ThinkingType_enabled.Enum(),
		}
		if effort != "" {
			ark.Reasoning = &arkresponses.ResponsesReasoning{
				Effort: arkReasoningEffort(effort),
			}
		}
	}
}

func arkServiceTier(tier string) *arkresponses.ResponsesServiceTier_Enum {
	if tier == "auto" {
		return arkresponses.ResponsesServiceTier_auto.Enum()
	}
	return arkresponses.ResponsesServiceTier_default.Enum()
}

func arkWebSearchTool(search *GenerateWebSearch) *arkresponses.ResponsesTool {
	tool := &arkresponses.ToolWebSearch{
		Type:       arkresponses.ToolType_web_search,
		Limit:      search.Limit,
		MaxKeyword: search.MaxKeyword,
	}
	for _, source := range search.Sources {
		tool.Sources = append(tool.Sources, arkSearchSource(source))
	}
	location := search.UserLocation
	if location.City != "" || location.Country != "" ||
		location.Region != "" || location.Timezone != "" {
		approximate := &arkresponses.UserLocation{
			Type: arkresponses.UserLocationType_approximate,
		}
		if location.City != "" {
			approximate.City = &location.City
		}
		if location.Country != "" {
			approximate.Country = &location.Country
		}
		if location.Region != "" {
			approximate.Region = &location.Region
		}
		if location.Timezone != "" {
			approximate.Timezone = &location.Timezone
		}
		tool.UserLocation = approximate
	}
	return &arkresponses.ResponsesTool{
		Union: &arkresponses.ResponsesTool_ToolWebSearch{ToolWebSearch: tool},
	}
}

func arkSearchSource(source string) arkresponses.SourceType_Enum {
	switch source {
	case "toutiao":
		return arkresponses.SourceType_toutiao
	case "douyin":
		return arkresponses.SourceType_douyin
	case "moji":
		return arkresponses.SourceType_moji
	default:
		return arkresponses.SourceType_search_engine
	}
}

func arkMessageRole(role string) arkresponses.MessageRole_Enum {
	if role == "assistant" {
		return arkresponses.MessageRole_assistant
	}
	return arkresponses.MessageRole_user
}

func arkTextFormat(kind, name string, schema []byte, strict bool) *arkresponses.TextFormat {
	switch kind {
	case "json_object":
		return &arkresponses.TextFormat{Type: arkresponses.TextType_json_object}
	case "json_schema":
		return &arkresponses.TextFormat{
			Type:   arkresponses.TextType_json_schema,
			Name:   name,
			Schema: &arkresponses.Bytes{Value: bytesClone(schema)},
			Strict: &strict,
		}
	}
	return nil
}

func arkReasoningEffort(effort string) arkresponses.ReasoningEffort_Enum {
	switch effort {
	case "minimal":
		return arkresponses.ReasoningEffort_minimal
	case "low":
		return arkresponses.ReasoningEffort_low
	case "high":
		return arkresponses.ReasoningEffort_high
	case "xhigh":
		return arkresponses.ReasoningEffort_high
	default:
		return arkresponses.ReasoningEffort_medium
	}
}

func arkToolChoice(choice inference.ToolChoice) *arkresponses.ResponsesToolChoice {
	switch choice.Mode {
	case inference.ToolChoiceNone:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_none},
		}
	case inference.ToolChoiceRequired:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_required},
		}
	case inference.ToolChoiceFunction:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_FunctionToolChoice{
				FunctionToolChoice: &arkresponses.FunctionToolChoice{
					Type: arkresponses.ToolType_function,
					Name: choice.Name,
				},
			},
		}
	default:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_auto},
		}
	}
}

func transportGenerate(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) protocol.Transport[inference.GenerateRequest, *arkresponses.ResponsesRequest, generateRaw] {
	return func(
		ctx context.Context,
		_ inference.GenerateRequest,
		request *arkresponses.ResponsesRequest,
	) (generateRaw, error) {
		response, err := client.CreateResponses(ctx, request, options...)
		if err != nil {
			return generateRaw{}, classifyError(err)
		}
		return arkToRaw(response)
	}
}

func arkToRaw(response *arkresponses.ResponseObject) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, fmt.Errorf("bytedance: empty responses object")
	}
	if failure := response.GetError(); failure != nil {
		return generateRaw{}, classifyResponseError(failure.GetCode(), failure.GetMessage())
	}
	raw := generateRaw{id: response.GetId()}
	for _, item := range response.GetOutput() {
		if reasoning := item.GetReasoning(); reasoning != nil {
			text := reasoningSummaryText(reasoning.GetSummary())
			if text == "" {
				continue
			}
			raw.reasonings = append(raw.reasonings, rawReasoning{id: reasoning.GetId(), text: text})
			continue
		}
		if message := item.GetOutputMessage(); message != nil {
			for _, content := range message.GetContent() {
				if text := content.GetText(); text != nil {
					raw.texts = append(raw.texts, text.GetText())
				}
			}
			continue
		}
		if call := item.GetFunctionToolCall(); call != nil {
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id: call.GetCallId(), name: call.GetName(), args: []byte(call.GetArguments()),
			})
		}
	}
	raw.usage = arkUsage(response.GetUsage())
	raw.finish = arkFinishReason(response, len(raw.toolCalls) > 0)
	return raw, nil
}

func arkUsage(raw *arkresponses.Usage) rawUsage {
	if raw == nil {
		return rawUsage{}
	}
	out := rawUsage{
		inputTokens:      raw.GetInputTokens(),
		outputTokens:     raw.GetOutputTokens(),
		cachedTokens:     raw.GetInputTokensDetails().GetCachedTokens(),
		reasoningTokens:  raw.GetOutputTokensDetails().GetReasoningTokens(),
		inputAudioTokens: raw.GetInputTokensDetails().GetAudioTokens(),
	}
	if tool := raw.GetToolUsage(); tool != nil {
		out.webSearchRequests = tool.GetWebSearch()
		out.mcpRequests = tool.GetMcp()
	}
	return out
}

func arkFinishReason(response *arkresponses.ResponseObject, hasToolCalls bool) inference.FinishReason {
	if hasToolCalls {
		return inference.FinishToolCalls
	}
	if incomplete := response.GetIncompleteDetails(); incomplete != nil {
		switch incomplete.GetReason() {
		case "max_output_tokens", "max_tokens":
			return inference.FinishLength
		case "content_filter":
			return inference.FinishContentFilter
		}
	}
	return inference.FinishStop
}

func reasoningSummaryText(summary []*arkresponses.ReasoningSummaryPart) string {
	texts := make([]string, 0, len(summary))
	for _, entry := range summary {
		if entry.GetText() != "" {
			texts = append(texts, entry.GetText())
		}
	}
	return strings.Join(texts, "\n\n")
}

func classifyResponseError(code, message string) error {
	err := fmt.Errorf("bytedance: response failed: %s %s", code, message)
	lower := strings.ToLower(code + " " + message)
	switch {
	case strings.Contains(lower, "rate"):
		return errdefs.RateLimit(err)
	case strings.Contains(lower, "auth"), strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "permission"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "filter"), strings.Contains(lower, "invalid"),
		strings.Contains(lower, "notfound"):
		return errdefs.Validation(err)
	default:
		return errdefs.NotAvailable(err)
	}
}

func bytesClone(raw []byte) []byte { return append([]byte(nil), raw...) }
