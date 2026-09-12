package bytedance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

// ---------------------------------------------------------------------------
// Wire → ark Responses API request. This conversion is total and pure: every
// field the compiler set has exactly one protobuf destination.
// ---------------------------------------------------------------------------

// The constructors below are the only place the compiler speaks ark's
// protobuf unions: each owns one wire shape and the compile loop reads as what
// the part means.

// appendInputItem adds one input item, creating the list on first use.
func appendInputItem(
	ark *arkresponses.ResponsesRequest,
	item *arkresponses.InputItem,
) {
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

func arkMessageItem(
	role string,
	content []*arkresponses.ContentItem,
) *arkresponses.InputItem {
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

// arkTool lowers one canonical tool definition.
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

// setArkThinking applies one reasoning decision: an explicit canonical switch
// wins, otherwise thinking follows the effort level. Neither set leaves the
// provider default in place.
func setArkThinking(
	ark *arkresponses.ResponsesRequest,
	enabled *bool,
	effort string,
) {
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

// arkServiceTier maps the extension token to the serving tier enum; the
// extension's Validate has already restricted values to auto/default.
func arkServiceTier(tier string) *arkresponses.ResponsesServiceTier_Enum {
	if tier == "auto" {
		return arkresponses.ResponsesServiceTier_auto.Enum()
	}
	return arkresponses.ResponsesServiceTier_default.Enum()
}

// arkWebSearchTool lowers the web search extension. The location is attached
// only when at least one field is set; the provider treats it as approximate.
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

// arkSearchSource maps one extension source token to its enum.
func arkSearchSource(source string) arkresponses.SourceType_Enum {
	switch source {
	case "toutiao":
		return arkresponses.SourceType_toutiao
	case "douyin":
		return arkresponses.SourceType_douyin
	case "moji":
		return arkresponses.SourceType_moji
	default: // "search_engine"
		return arkresponses.SourceType_search_engine
	}
}

func arkMessageRole(role string) arkresponses.MessageRole_Enum {
	if role == "assistant" {
		return arkresponses.MessageRole_assistant
	}
	return arkresponses.MessageRole_user
}

func arkTextFormat(
	kind, name string,
	schema []byte,
	strict bool,
) *arkresponses.TextFormat {
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
	case "low":
		return arkresponses.ReasoningEffort_low
	case "high":
		return arkresponses.ReasoningEffort_high
	default:
		return arkresponses.ReasoningEffort_medium
	}
}

func arkToolChoice(choice inference.ToolChoice) *arkresponses.ResponsesToolChoice {
	switch choice.Kind {
	case inference.ToolChoiceNone:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_none},
		}
	case inference.ToolChoiceRequired:
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_required},
		}
	case inference.ToolChoiceNamed:
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

// ---------------------------------------------------------------------------
// Unary transport and decode.
// ---------------------------------------------------------------------------

func transportGenerate(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) inference.Transport[*arkresponses.ResponsesRequest, generateRaw] {
	return func(ctx context.Context, request *arkresponses.ResponsesRequest) (generateRaw, error) {
		response, err := client.CreateResponses(ctx, request, options...)
		if err != nil {
			classified := classifyError(err)
			inference.LogProviderCall(ctx, providerID, "generate", request.Model, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := arkToRaw(response)
		if err != nil {
			inference.LogProviderCall(ctx, providerID, "generate", request.Model, err, "", "")
			return generateRaw{}, err
		}
		inference.LogProviderCall(ctx, providerID, "generate", request.Model, nil, "", raw.id)
		return raw, nil
	}
}

// arkToRaw converts the protobuf response into the provider-owned raw model,
// rejecting provider failures with classified errors.
func arkToRaw(response *arkresponses.ResponseObject) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, fmt.Errorf("bytedance: empty responses object")
	}
	if failure := response.GetError(); failure != nil {
		return generateRaw{}, classifyResponseError(
			failure.GetCode(),
			failure.GetMessage(),
		)
	}
	raw := generateRaw{id: response.GetId()}
	for _, item := range response.GetOutput() {
		if reasoning := item.GetReasoning(); reasoning != nil {
			text := reasoningSummaryText(reasoning.GetSummary())
			// An id-only item carries no visible trace and ark signs
			// nothing, so it is pure noise — the canonical part requires
			// content.
			if text == "" {
				continue
			}
			raw.reasonings = append(raw.reasonings, rawReasoning{
				id:   reasoning.GetId(),
				text: text,
			})
			continue
		}
		if message := item.GetOutputMessage(); message != nil {
			for _, content := range message.GetContent() {
				if text := content.GetText(); text != nil {
					raw.texts = append(raw.texts, text.GetText())
					raw.citations = append(raw.citations,
						arkCitations(text.GetAnnotations())...)
				}
			}
			continue
		}
		if call := item.GetFunctionToolCall(); call != nil {
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   call.GetCallId(),
				name: call.GetName(),
				args: []byte(call.GetArguments()),
			})
		}
		if call := item.GetFunctionWebSearch(); call != nil {
			raw.webSearchCalls = append(raw.webSearchCalls,
				arkWebSearchCall(call))
		}
	}
	raw.usage = arkUsage(response.GetUsage())
	raw.finish = arkFinishReason(response, len(raw.toolCalls) > 0)
	return raw, nil
}

func arkWebSearchCall(call *arkresponses.ItemFunctionWebSearch) inference.WebSearchCall {
	record := inference.WebSearchCall{
		ID:     call.GetId(),
		Status: call.GetStatus().String(),
	}
	if action := call.GetAction(); action != nil {
		record.Action = action.GetType().String()
		record.Queries = append(record.Queries, action.GetQuery())
	}
	return record
}

func arkCitations(annotations []*arkresponses.Annotation) []inference.Citation {
	citations := make([]inference.Citation, 0, len(annotations))
	for _, annotation := range annotations {
		citation := inference.Citation{
			URL:         annotation.GetUrl(),
			Title:       annotation.GetTitle(),
			SiteName:    annotation.GetSiteName(),
			PublishTime: annotation.GetPublishTime(),
		}
		if citation.URL == "" {
			continue
		}
		citations = append(citations, citation)
	}
	return citations
}

func arkUsage(usage *arkresponses.Usage) rawUsage {
	raw := rawUsage{
		inputTokens:      usage.GetInputTokens(),
		outputTokens:     usage.GetOutputTokens(),
		totalTokens:      usage.GetTotalTokens(),
		cachedTokens:     usage.GetInputTokensDetails().GetCachedTokens(),
		reasoningTokens:  usage.GetOutputTokensDetails().GetReasoningTokens(),
		inputAudioTokens: usage.GetInputTokensDetails().GetAudioTokens(),
	}
	if tool := usage.GetToolUsage(); tool != nil {
		raw.webSearchRequests = tool.GetWebSearch()
		raw.mcpRequests = tool.GetMcp()
	}
	if raw.totalTokens == 0 {
		raw.totalTokens = raw.inputTokens + raw.outputTokens
	}
	return raw
}

func arkFinishReason(
	response *arkresponses.ResponseObject,
	hasToolCalls bool,
) inference.FinishReason {
	if hasToolCalls {
		return inference.FinishToolCalls
	}
	if incomplete := response.GetIncompleteDetails(); incomplete != nil {
		switch incomplete.GetReason() {
		case "max_output_tokens", "max_tokens":
			return inference.FinishMaxOutput
		case "content_filter":
			return inference.FinishContentFilter
		}
	}
	return inference.FinishCompleted
}

// reasoningSummaryText joins one reasoning item's summary entries. The
// canonical part is item-granular, so visible summary text joins with a
// blank line.
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

func decodeGenerate(
	_ context.Context,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	if raw.failedReason != "" {
		return inference.GenerateResponse{}, fmt.Errorf(
			"bytedance: response failed: %s",
			raw.failedReason,
		)
	}
	parts := make([]message.Part, 0,
		len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	// ark emits reasoning items before the answer; the canonical message
	// keeps that order.
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{
			Text: reasoning.text,
			ID:   reasoning.id,
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
	response := inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Usage:        rawUsageCanonical(raw.usage),
		Metadata:     inference.Metadata{ResponseID: raw.id},
	}
	if output := webSearchProviderOutput(raw.webSearchCalls, raw.citations); output != nil {
		response.ProviderOutputs = append(response.ProviderOutputs, output)
	}
	return response, nil
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
		// Ark reports output_tokens inclusive of reasoning tokens.
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	if raw.inputAudioTokens > 0 {
		usage.Input.ByModality = append(usage.Input.ByModality,
			inference.ModalityTokenUsage{
				Modality: inference.ModalityAudio,
				Tokens:   raw.inputAudioTokens,
			})
	}
	if raw.webSearchRequests > 0 {
		usage.Tools = append(usage.Tools, inference.ToolUsage{
			Kind:     inference.ToolUsageWebSearch,
			Requests: raw.webSearchRequests,
		})
	}
	if raw.mcpRequests > 0 {
		usage.Tools = append(usage.Tools, inference.ToolUsage{
			Kind:     inference.ToolUsageMCP,
			Requests: raw.mcpRequests,
		})
	}
	return usage
}
