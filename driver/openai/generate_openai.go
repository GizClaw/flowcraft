package openai

// Responses surface: the sink that spells the compiler's decisions as
// Responses params, plus the transport and the response decoder. The compiler
// lives in generate.go; this file owns everything that needs the SDK type.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// compileResponses lowers a canonical request into Responses params.
func compileResponses(
	modelName string,
	entry catalogEntry,
) inference.GenerateCompiler[*responsesRequest] {
	return compileGenerate(entry, func(inference.GenerateExecutionShape) *responsesRequest {
		return newResponsesRequest(modelName, entry)
	})
}

// responsesRequest is one compiled Responses call: the SDK params the
// transport posts, plus the per-request options for the fields the API leaves
// untyped (an aliased metadata envelope). Both halves are SDK values, so
// nothing here describes the request a second time.
type responsesRequest struct {
	params  responses.ResponseNewParams
	options []option.RequestOption
	// hasToolChoice remembers that the intent already picked a tool choice,
	// so the hosted web_search requirement does not overwrite it.
	hasToolChoice bool
}

// newResponsesRequest seeds the provider-wide policy of one Responses call:
// everything the deployment's wire spec and the model's declared capabilities
// decide before the request is read.
func newResponsesRequest(
	modelName string,
	entry catalogEntry,
) *responsesRequest {
	request := &responsesRequest{
		params: responses.ResponseNewParams{Model: modelName},
	}
	// The OpenAI default is store: true, which retains the response for at
	// least 30 days. FlowCraft replays context itself, so the driver states
	// the decision instead of inheriting it.
	request.params.Store = param.NewOpt(entry.dialect.store)
	// Summaries are opt-in: without this the provider returns the encrypted
	// payload and no readable trace.
	if summary := entry.dialect.reasoningSummary; summary != "" {
		request.params.Reasoning.Summary = shared.ReasoningSummary(summary)
	}
	if mode := entry.dialect.truncation; mode != "" {
		request.params.Truncation = responses.ResponseNewParamsTruncation(mode)
	}
	// Reasoning traces are worthless to consumers without their encrypted
	// payload: without it the reasoning cannot round-trip into later context,
	// which breaks agent loops silently. Only reasoning models accept the
	// include; Azure rejects it on plain chat deployments.
	if entry.capabilities.Reasoning.Kind != model.ReasoningNone &&
		!entry.dialect.omitReasoningPayload {
		request.params.Include = []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		}
	}
	return request
}

// ---------------------------------------------------------------------------
// Sink
// ---------------------------------------------------------------------------

func (r *responsesRequest) message(role string, content []contentPart) {
	item := responses.ResponseInputItemUnionParam{}
	if role == string(message.RoleAssistant) {
		// Assistant text rides an output-message item: the Responses API
		// accepts output_text and refusal under the assistant role only, and
		// the SDK's EasyInputMessage content union can express neither, so an
		// assistant item built that way carries input_text and the provider
		// rejects the whole request (400 invalid_value).
		item.OfOutputMessage = &responses.ResponseOutputMessageParam{
			Content: assistantOutputContent(content),
		}
	} else {
		item.OfMessage = &responses.EasyInputMessageParam{
			Role:    responses.EasyInputMessageRole(role),
			Content: messageContent(content),
		}
	}
	r.appendItem(item)
}

func (r *responsesRequest) toolCall(callID, name string, args []byte) {
	r.appendItem(responses.ResponseInputItemUnionParam{
		OfFunctionCall: &responses.ResponseFunctionToolCallParam{
			CallID:    callID,
			Name:      name,
			Arguments: string(args),
		},
	})
}

func (r *responsesRequest) toolResult(callID string, content []contentPart) {
	r.appendItem(responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: callID,
			Output: functionCallOutputParam(content),
		},
	})
}

func (r *responsesRequest) reasoning(trace message.ReasoningPart, plain bool) {
	if plain {
		// A plain-text reasoning channel round-trips the trace in the content
		// list: summary and encrypted payload are not part of its contract,
		// and sending them would describe a round-trip the endpoint does not
		// verify.
		r.appendItem(responses.ResponseInputItemUnionParam{
			OfReasoning: &responses.ResponseReasoningItemParam{
				ID: trace.ID,
				Content: []responses.ResponseReasoningItemContentParam{
					{Text: trace.Text},
				},
			},
		})
		return
	}
	reasoning := responses.ResponseReasoningItemParam{
		ID:               trace.ID,
		EncryptedContent: param.NewOpt(trace.Signature),
		// The API requires the summary field on a replayed reasoning item even
		// when it carries no summary text: summaries are opt-in via
		// reasoning.summary, so an empty array is the normal shape. Omitting
		// the field fails the whole request with missing_required_parameter.
		Summary: []responses.ResponseReasoningItemSummaryParam{},
	}
	if trace.Text != "" {
		reasoning.Summary = []responses.ResponseReasoningItemSummaryParam{
			{Text: trace.Text},
		}
	}
	r.appendItem(responses.ResponseInputItemUnionParam{OfReasoning: &reasoning})
}

func (r *responsesRequest) setTextFormat(format *inference.ResponseFormat) {
	switch format.Kind {
	case inference.ResponseJSONObject:
		r.params.Text.Format = responses.ResponseFormatTextConfigUnionParam{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	case inference.ResponseJSONSchema:
		r.params.Text.Format = responses.ResponseFormatTextConfigParamOfJSONSchema(
			format.Name,
			schemaMap(format.Schema),
		)
	}
}

func (r *responsesRequest) setMaxOutputTokens(tokens int64) {
	r.params.MaxOutputTokens = param.NewOpt(tokens)
}

func (r *responsesRequest) setTemperature(value float64) {
	r.params.Temperature = param.NewOpt(value)
}

func (r *responsesRequest) setTopP(value float64) {
	r.params.TopP = param.NewOpt(value)
}

func (r *responsesRequest) addTool(definition message.ToolDefinition) {
	tool := responses.FunctionToolParam{
		Name:       definition.Name,
		Parameters: schemaMap(definition.InputSchema),
	}
	if definition.Description != "" {
		tool.Description = param.NewOpt(definition.Description)
	}
	r.params.Tools = append(r.params.Tools, responses.ToolUnionParam{
		OfFunction: &tool,
	})
}

func (r *responsesRequest) setToolChoice(choice inference.ToolChoice) {
	r.hasToolChoice = true
	switch choice.Kind {
	case inference.ToolChoiceNone:
		r.params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsNone),
		}
	case inference.ToolChoiceRequired:
		r.params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired),
		}
	case inference.ToolChoiceNamed:
		r.params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice.Name},
		}
	case inference.ToolChoiceAuto:
		r.params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto),
		}
	}
}

func (r *responsesRequest) setReasoningEffort(effort string) {
	r.params.Reasoning.Effort = shared.ReasoningEffort(effort)
}

func (r *responsesRequest) setVerbosity(level string) {
	r.params.Text.Verbosity = responses.ResponseTextConfigVerbosity(level)
}

func (r *responsesRequest) setServiceTier(tier string) {
	r.params.ServiceTier = responses.ResponseNewParamsServiceTier(tier)
}

func (r *responsesRequest) setParallelToolCalls(value bool) {
	r.params.ParallelToolCalls = param.NewOpt(value)
}

func (r *responsesRequest) setMaxToolCalls(calls int) {
	r.params.MaxToolCalls = param.NewOpt(int64(calls))
}

func (r *responsesRequest) setSafetyIdentifier(identifier string) {
	r.params.SafetyIdentifier = param.NewOpt(identifier)
}

func (r *responsesRequest) setPromptCacheKey(key string) {
	r.params.PromptCacheKey = param.NewOpt(key)
}

func (r *responsesRequest) setRequestMetadata(
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

func (r *responsesRequest) addHostedWebSearch(
	search *GenerateWebSearch,
	required bool,
) {
	tool := webSearchToolParam(search)
	r.params.Tools = append(r.params.Tools, responses.ToolUnionParam{
		OfWebSearch: &tool,
	})
	// Sources only come back when the caller asks for them.
	r.params.Include = append(r.params.Include,
		responses.ResponseIncludableWebSearchCallActionSources)
	if required && !r.hasToolChoice {
		r.params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired),
		}
	}
}

func (r *responsesRequest) appendItem(item responses.ResponseInputItemUnionParam) {
	r.params.Input.OfInputItemList = append(r.params.Input.OfInputItemList, item)
}

// webSearchToolParam lowers the caller's hosted-search options. The SDK types
// the common fields; the two newest knobs ride ExtraFields.
func webSearchToolParam(search *GenerateWebSearch) responses.WebSearchToolParam {
	tool := responses.WebSearchToolParam{
		Type: responses.WebSearchToolTypeWebSearch,
	}
	if search.SearchContextSize != "" {
		tool.SearchContextSize =
			responses.WebSearchToolSearchContextSize(search.SearchContextSize)
	}
	if len(search.AllowedDomains) > 0 {
		tool.Filters = responses.WebSearchToolFiltersParam{
			AllowedDomains: append([]string(nil), search.AllowedDomains...),
		}
	}
	if location := search.UserLocation; location.City != "" ||
		location.Country != "" || location.Region != "" || location.Timezone != "" {
		tool.UserLocation = responses.WebSearchToolUserLocationParam{
			Type:     "approximate",
			City:     param.NewOpt(location.City),
			Country:  param.NewOpt(location.Country),
			Region:   param.NewOpt(location.Region),
			Timezone: param.NewOpt(location.Timezone),
		}
	}
	extra := map[string]any{}
	if search.ExternalWebAccess != nil {
		extra["external_web_access"] = *search.ExternalWebAccess
	}
	if search.ReturnTokenBudget != "" {
		extra["return_token_budget"] = search.ReturnTokenBudget
	}
	if len(extra) > 0 {
		tool.SetExtraFields(extra)
	}
	return tool
}

// functionCallOutputParam lowers a tool result. A single text part keeps the
// string form every endpoint and model accepts verbatim; anything richer rides
// the content-list form the Responses API defines for tool output.
func functionCallOutputParam(
	result []contentPart,
) responses.ResponseInputItemFunctionCallOutputOutputUnionParam {
	if len(result) <= 1 &&
		(len(result) == 0 || result[0].kind == contentText) {
		text := ""
		if len(result) == 1 {
			text = result[0].text
		}
		return responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
			OfString: param.NewOpt(text),
		}
	}
	list := make(responses.ResponseFunctionCallOutputItemListParam, 0, len(result))
	for _, part := range result {
		if part.kind == contentImage {
			list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{
				OfInputImage: &responses.ResponseInputImageContentParam{
					ImageURL: param.NewOpt(part.uri),
				},
			})
			continue
		}
		list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{
			OfInputText: &responses.ResponseInputTextContentParam{
				Text: part.text,
			},
		})
	}
	return responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
		OfResponseFunctionCallOutputItemArray: list,
	}
}

func messageContent(
	content []contentPart,
) responses.EasyInputMessageContentUnionParam {
	list := make(responses.ResponseInputMessageContentListParam, 0, len(content))
	for _, part := range content {
		if part.kind == contentImage {
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: param.NewOpt(part.uri),
				},
			})
			continue
		}
		list = append(list, responses.ResponseInputContentUnionParam{
			OfInputText: &responses.ResponseInputTextParam{Text: part.text},
		})
	}
	return responses.EasyInputMessageContentUnionParam{OfInputItemContentList: list}
}

// assistantOutputContent lowers assistant parts to output_text. The compiler
// rejects media on assistant turns, so text is the only kind that arrives
// here; anything else is skipped rather than marshalled into a shape the
// provider would refuse.
func assistantOutputContent(
	content []contentPart,
) []responses.ResponseOutputMessageContentUnionParam {
	parts := make([]responses.ResponseOutputMessageContentUnionParam, 0, len(content))
	for _, part := range content {
		if part.kind != contentText {
			continue
		}
		parts = append(parts, responses.ResponseOutputMessageContentUnionParam{
			OfOutputText: &responses.ResponseOutputTextParam{
				Text: part.text,
				// Annotations are declared required on the output-text shape,
				// and a replayed turn has none to carry.
				Annotations: []responses.ResponseOutputTextAnnotationUnionParam{},
			},
		})
	}
	return parts
}

// ---------------------------------------------------------------------------
// Unary transport and decode.
// ---------------------------------------------------------------------------

func transportGenerate(
	client openai.Client,
) inference.Transport[*responsesRequest, generateRaw] {
	return func(ctx context.Context, request *responsesRequest) (generateRaw, error) {
		modelName := string(request.params.Model)
		response, err := client.Responses.New(ctx, request.params, request.options...)
		if err != nil {
			classified := classifyError(err)
			inference.LogProviderCall(ctx, providerID, "generate", modelName, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := responseToRaw(response)
		if err != nil {
			inference.LogProviderCall(ctx, providerID, "generate", modelName, err, "", "")
			return generateRaw{}, err
		}
		inference.LogProviderCall(ctx, providerID, "generate", modelName, nil, "", raw.id)
		return raw, nil
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
			// Summary is the OpenAI shape; a plain-text reasoning channel
			// carries the trace in the content list instead, and either may
			// arrive alone.
			raw.reasonings = append(raw.reasonings, rawReasoning{
				id:        item.ID,
				text:      reasoningText(item.AsReasoning()),
				signature: item.EncryptedContent,
			})
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" {
					raw.texts = append(raw.texts, content.Text)
					raw.citations = append(raw.citations,
						openaiCitations(content.Annotations)...)
				}
			}
		case "function_call":
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   item.CallID,
				name: item.Name,
				args: []byte(item.Arguments.OfString),
			})
		case "web_search_call":
			raw.webSearchCalls = append(raw.webSearchCalls,
				openaiWebSearchCall(item))
		}
	}
	raw.usage = responseUsage(response.Usage)
	raw.finish = responseFinish(response, len(raw.toolCalls) > 0)
	return raw, nil
}

func openaiWebSearchCall(
	item responses.ResponseOutputItemUnion,
) inference.WebSearchCall {
	call := item.AsWebSearchCall()
	record := inference.WebSearchCall{
		ID:     call.ID,
		Status: string(call.Status),
	}
	switch call.Action.Type {
	case "search":
		action := call.Action.AsSearch()
		record.Action = string(action.Type)
		record.Queries = append([]string(nil), action.Queries...)
		for _, source := range action.Sources {
			record.Sources = append(record.Sources, source.URL)
		}
	case "open_page":
		action := call.Action.AsOpenPage()
		record.Action = string(action.Type)
		record.Sources = append(record.Sources, action.URL)
	case "find_in_page":
		action := call.Action.AsFind()
		record.Action = string(action.Type)
		record.Queries = append(record.Queries, action.Pattern)
		record.Sources = append(record.Sources, action.URL)
	}
	return record
}

func openaiCitations(
	annotations []responses.ResponseOutputTextAnnotationUnion,
) []inference.Citation {
	citations := make([]inference.Citation, 0, len(annotations))
	for _, annotation := range annotations {
		if annotation.Type != "url_citation" {
			continue
		}
		url := annotation.AsURLCitation()
		citation := inference.Citation{
			URL:   url.URL,
			Title: url.Title,
		}
		start, end := url.StartIndex, url.EndIndex
		citation.StartIndex = &start
		citation.EndIndex = &end
		citations = append(citations, citation)
	}
	return citations
}

func responseUsage(usage responses.ResponseUsage) rawUsage {
	raw := rawUsage{
		inputTokens:      usage.InputTokens,
		outputTokens:     usage.OutputTokens,
		totalTokens:      usage.TotalTokens,
		cachedTokens:     usage.InputTokensDetails.CachedTokens,
		cacheWriteTokens: usage.InputTokensDetails.CacheWriteTokens,
		reasoningTokens:  usage.OutputTokensDetails.ReasoningTokens,
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

// reasoningText returns the reasoning item's visible text. OpenAI carries it
// as summary entries; a plain-text reasoning channel returns reasoning_text
// content parts instead. Summary wins when both are present, because it is
// the shape the caller opted into.
func reasoningText(item responses.ResponseReasoningItem) string {
	if text := reasoningSummary(item.Summary); text != "" {
		return text
	}
	var builder strings.Builder
	for _, content := range item.Content {
		if content.Type == "reasoning_text" {
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
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
	if raw.cacheWriteTokens > 0 {
		write := raw.cacheWriteTokens
		usage.Input.CacheWriteTokens = &write
	}
	if raw.reasoningTokens > 0 {
		reasoning := raw.reasoningTokens
		usage.Output.ReasoningTokens = &reasoning
		// The Responses API reports output_tokens inclusive of reasoning.
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	if raw.acceptedPredictionTokens > 0 {
		accepted := raw.acceptedPredictionTokens
		usage.Output.AcceptedPredictionTokens = &accepted
	}
	if raw.rejectedPredictionTokens > 0 {
		rejected := raw.rejectedPredictionTokens
		usage.Output.RejectedPredictionTokens = &rejected
	}
	if raw.inputAudioTokens > 0 {
		usage.Input.ByModality = append(usage.Input.ByModality,
			inference.ModalityTokenUsage{
				Modality: inference.ModalityAudio,
				Tokens:   raw.inputAudioTokens,
			})
	}
	if raw.outputAudioTokens > 0 {
		usage.Output.ByModality = append(usage.Output.ByModality,
			inference.ModalityTokenUsage{
				Modality: inference.ModalityAudio,
				Tokens:   raw.outputAudioTokens,
			})
	}
	return usage
}
