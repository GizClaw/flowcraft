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

func wireToArk(wire generateWire) *arkresponses.ResponsesRequest {
	list := &arkresponses.InputItemList{}
	for _, item := range wire.items {
		switch item.kind {
		case wireItemMessage:
			list.ListValue = append(list.ListValue, &arkresponses.InputItem{
				Union: &arkresponses.InputItem_EasyMessage{
					EasyMessage: &arkresponses.ItemEasyMessage{
						Type:    arkresponses.ItemType_message.Enum(),
						Role:    arkMessageRole(item.role),
						Content: arkMessageContent(item.content),
					},
				},
			})
		case wireItemToolCall:
			list.ListValue = append(list.ListValue, &arkresponses.InputItem{
				Union: &arkresponses.InputItem_FunctionToolCall{
					FunctionToolCall: &arkresponses.ItemFunctionToolCall{
						Type:      arkresponses.ItemType_function_call,
						CallId:    item.callID,
						Name:      item.name,
						Arguments: string(item.args),
					},
				},
			})
		case wireItemToolResult:
			list.ListValue = append(list.ListValue, &arkresponses.InputItem{
				Union: &arkresponses.InputItem_FunctionToolCallOutput{
					FunctionToolCallOutput: &arkresponses.ItemFunctionToolCallOutput{
						Type:   arkresponses.ItemType_function_call_output,
						CallId: item.callID,
						Output: item.output,
					},
				},
			})
		}
	}

	request := &arkresponses.ResponsesRequest{
		Input: &arkresponses.ResponsesInput{
			Union: &arkresponses.ResponsesInput_ListValue{ListValue: list},
		},
		Model:           wire.model,
		MaxOutputTokens: wire.maxTokens,
		Temperature:     wire.temperature,
		TopP:            wire.topP,
	}
	if wire.instructions != "" {
		request.Instructions = &wire.instructions
	}
	if wire.stream {
		stream := true
		request.Stream = &stream
	}
	if wire.textFormat != nil {
		request.Text = &arkresponses.ResponsesText{Format: arkTextFormat(wire.textFormat)}
	}
	// Thinking: an explicit canonical switch wins; otherwise thinking follows
	// the reasoning effort. Neither set leaves the provider default in place.
	switch {
	case wire.thinking != nil && !*wire.thinking:
		request.Thinking = &arkresponses.ResponsesThinking{
			Type: arkresponses.ThinkingType_disabled.Enum(),
		}
	case wire.reasoning != nil || (wire.thinking != nil && *wire.thinking):
		request.Thinking = &arkresponses.ResponsesThinking{
			Type: arkresponses.ThinkingType_enabled.Enum(),
		}
		if wire.reasoning != nil {
			request.Reasoning = &arkresponses.ResponsesReasoning{
				Effort: arkReasoningEffort(wire.reasoning.effort),
			}
		}
	}
	if wire.serviceTier != "" {
		request.ServiceTier = arkServiceTier(wire.serviceTier)
	}
	if wire.caching != nil {
		cacheType := arkresponses.CacheType_disabled
		if wire.caching.enabled {
			cacheType = arkresponses.CacheType_enabled
		}
		request.Caching = &arkresponses.ResponsesCaching{
			Type:   cacheType.Enum(),
			Prefix: &wire.caching.prefix,
		}
	}
	if wire.store != nil {
		request.Store = wire.store
	}
	if wire.previousResponseID != "" {
		request.PreviousResponseId = &wire.previousResponseID
	}
	if wire.parallelToolCalls != nil {
		request.ParallelToolCalls = wire.parallelToolCalls
	}
	if wire.maxToolCalls != nil {
		request.MaxToolCalls = wire.maxToolCalls
	}
	for _, t := range wire.tools {
		definition := &arkresponses.ToolFunction{
			Type:       arkresponses.ToolType_function,
			Name:       t.name,
			Parameters: &arkresponses.Bytes{Value: t.schema},
		}
		if t.description != "" {
			definition.Description = &t.description
		}
		request.Tools = append(request.Tools, &arkresponses.ResponsesTool{
			Union: &arkresponses.ResponsesTool_ToolFunction{ToolFunction: definition},
		})
	}
	if wire.toolChoice != nil {
		request.ToolChoice = arkToolChoice(wire.toolChoice)
	}
	if wire.webSearch != nil {
		request.Tools = append(request.Tools, &arkresponses.ResponsesTool{
			Union: &arkresponses.ResponsesTool_ToolWebSearch{
				ToolWebSearch: arkWebSearch(wire.webSearch),
			},
		})
	}
	return request
}

// arkServiceTier maps the extension token to the serving tier enum; the
// extension's Validate has already restricted values to auto/default.
func arkServiceTier(tier string) *arkresponses.ResponsesServiceTier_Enum {
	if tier == "auto" {
		return arkresponses.ResponsesServiceTier_auto.Enum()
	}
	return arkresponses.ResponsesServiceTier_default.Enum()
}

// arkWebSearch lowers the wire web search config. The location is attached
// only when at least one field is set; the provider treats it as approximate.
func arkWebSearch(search *wireWebSearch) *arkresponses.ToolWebSearch {
	tool := &arkresponses.ToolWebSearch{
		Type:       arkresponses.ToolType_web_search,
		Limit:      search.limit,
		MaxKeyword: search.maxKeyword,
	}
	for _, source := range search.sources {
		tool.Sources = append(tool.Sources, arkSearchSource(source))
	}
	if search.city != "" || search.country != "" ||
		search.region != "" || search.timezone != "" {
		location := &arkresponses.UserLocation{
			Type: arkresponses.UserLocationType_approximate,
		}
		if search.city != "" {
			location.City = &search.city
		}
		if search.country != "" {
			location.Country = &search.country
		}
		if search.region != "" {
			location.Region = &search.region
		}
		if search.timezone != "" {
			location.Timezone = &search.timezone
		}
		tool.UserLocation = location
	}
	return tool
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

func arkMessageContent(content []wireContent) *arkresponses.MessageContent {
	items := make([]*arkresponses.ContentItem, 0, len(content))
	for _, part := range content {
		switch part.kind {
		case wireContentText:
			items = append(items, &arkresponses.ContentItem{
				Union: &arkresponses.ContentItem_Text{
					Text: &arkresponses.ContentItemText{
						Type: arkresponses.ContentItemType_input_text,
						Text: part.text,
					},
				},
			})
		case wireContentImage:
			uri := part.uri
			items = append(items, &arkresponses.ContentItem{
				Union: &arkresponses.ContentItem_Image{
					Image: &arkresponses.ContentItemImage{
						Type:     arkresponses.ContentItemType_input_image,
						ImageUrl: &uri,
					},
				},
			})
		case wireContentVideo:
			items = append(items, &arkresponses.ContentItem{
				Union: &arkresponses.ContentItem_Video{
					Video: &arkresponses.ContentItemVideo{
						Type:     arkresponses.ContentItemType_input_video,
						VideoUrl: part.uri,
					},
				},
			})
		}
	}
	return &arkresponses.MessageContent{
		Union: &arkresponses.MessageContent_ListValue{
			ListValue: &arkresponses.ContentItemList{ListValue: items},
		},
	}
}

func arkTextFormat(format *wireTextFormat) *arkresponses.TextFormat {
	switch format.kind {
	case "json_object":
		return &arkresponses.TextFormat{Type: arkresponses.TextType_json_object}
	case "json_schema":
		strict := format.strict
		return &arkresponses.TextFormat{
			Type:   arkresponses.TextType_json_schema,
			Name:   format.name,
			Schema: &arkresponses.Bytes{Value: format.schema},
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

func arkToolChoice(choice *wireToolChoice) *arkresponses.ResponsesToolChoice {
	switch choice.mode {
	case "none":
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_none},
		}
	case "required":
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_required},
		}
	case "named":
		return &arkresponses.ResponsesToolChoice{
			Union: &arkresponses.ResponsesToolChoice_FunctionToolChoice{
				FunctionToolChoice: &arkresponses.FunctionToolChoice{
					Type: arkresponses.ToolType_function,
					Name: choice.name,
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

func transportGenerate(client *arkruntime.Client) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		response, err := client.CreateResponses(ctx, wireToArk(wire))
		if err != nil {
			classified := classifyError(err)
			logInferenceCall(ctx, "generate", wire.model, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := arkToRaw(response)
		if err != nil {
			logInferenceCall(ctx, "generate", wire.model, err, "", "")
			return generateRaw{}, err
		}
		logInferenceCall(ctx, "generate", wire.model, nil, "", raw.id)
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
