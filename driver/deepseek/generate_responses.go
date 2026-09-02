package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// responseWire is the provider-owned intermediate representation for one
// Responses API request. The compiler lowers canonical requests into plain
// Go values; only the transport converts them into openai-go params.
type responseWire struct {
	model string
	items []responseWireItem

	textFormat  *wireTextFormat
	maxTokens   *int64
	temperature *float64
	topP        *float64
	reasoning   string // effort; "none" disables thinking; empty means unset
	tools       []wireTool
	toolChoice  *wireToolChoice
	webSearch   *wireWebSearch
	stream      bool

	// requestMetadataEnvelope names the top-level body field that carries
	// canonical request metadata; empty disables forwarding.
	requestMetadataEnvelope string
	// requestMetadata is the opaque metadata bag forwarded verbatim.
	requestMetadata map[string]string
}

type responseWireItemKind string

const (
	responseWireItemMessage    responseWireItemKind = "message"
	responseWireItemToolCall   responseWireItemKind = "tool_call"
	responseWireItemToolResult responseWireItemKind = "tool_result"
	responseWireItemReasoning  responseWireItemKind = "reasoning"
)

type responseWireItem struct {
	kind    responseWireItemKind
	role    string // message: system | user | assistant
	content []wireContent
	callID  string // tool_call / tool_result
	name    string // tool_call
	args    []byte // tool_call: JSON object
	output  string // tool_result

	// reasoning carries one DeepSeek reasoning item round-trip: the item id
	// (when the canonical part has one) and the plain reasoning text.
	reasoningID   string
	reasoningText string
}

type wireContentKind string

const (
	wireContentText  wireContentKind = "text"
	wireContentImage wireContentKind = "image"
)

type wireContent struct {
	kind wireContentKind
	text string
	// uri carries an absolute URL or a data: URI assembled from inline
	// bytes. It is only populated for image content.
	uri string
}

type wireTextFormat struct {
	kind   string // json_object | json_schema
	name   string
	schema []byte
	strict bool
}

type wireWebSearch struct {
	searchContextSize string
	allowedDomains    []string
	city              string
	country           string
	region            string
	timezone          string
	externalWebAccess *bool
	returnTokenBudget string
	required          bool
}

// compileResponsesGenerate lowers a canonical request into the DeepSeek
// Responses wire. DeepSeek's surface is OpenAI-compatible but has its own
// rules: plain-text reasoning items (no encrypted payload), no include,
// image input on the vision model (user messages only), and hosted
// web_search on supported models.
func compileResponsesGenerate(
	model string,
	entry catalogEntry,
) inference.GenerateCompiler[responseWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[responseWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := responseWire{
			model:  model,
			stream: shape == inference.GenerateExecutionStream,
		}
		if entry.requestMetadataEnvelope != "" && len(request.RequestMetadata) > 0 {
			wire.requestMetadataEnvelope = entry.requestMetadataEnvelope
			wire.requestMetadata = maps.Clone(request.RequestMetadata)
		} else if len(request.RequestMetadata) > 0 {
			ledger.drop(
				inference.FieldGenerateRequestMetadata,
				"deepseek request_metadata forwarding is disabled (set spec.request_metadata.envelope)",
			)
		}

		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleTool:
				compileResponsesToolResults(&wire, turn.Content.Parts, contextPartFields, ledger)
			default:
				compileResponsesMessage(
					&wire,
					string(turn.Role),
					turn.Content.Parts,
					entry,
					contextPartFields,
					ledger,
				)
			}
		}
		if request.Input.Role == inference.InputRoleTool {
			compileResponsesToolResults(
				&wire,
				request.Input.Content.Parts,
				inputPartFields,
				ledger,
			)
		} else {
			compileResponsesMessage(
				&wire,
				"user",
				request.Input.Content.Parts,
				entry,
				inputPartFields,
				ledger,
			)
		}

		compileResponsesIntent(&wire, request.Input.Content.Intent, entry, ledger)

		options, other := operationExtensions[GenerateOptions](request.Extensions)
		rejectOtherExtensions("generate", other, ledger)
		compileResponsesGenerateOptions(&wire, options, entry, ledger)

		report := ledger.report()
		if err := ledger.err(); err != nil {
			return inference.Compiled[responseWire]{Report: report}, err
		}
		return inference.Compiled[responseWire]{Wire: wire, Report: report}, nil
	}
}

// compileResponsesMessage appends one message's parts to the wire. The
// Responses item model separates function calls from messages, so a message
// with interleaved text and tool parts becomes a run of message items plus
// call items in original order.
func compileResponsesMessage(
	wire *responseWire,
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	var content []wireContent
	flush := func() {
		if len(content) == 0 {
			return
		}
		wire.items = append(wire.items, responseWireItem{
			kind:    responseWireItemMessage,
			role:    role,
			content: content,
		})
		content = nil
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			content = append(content, wireContent{
				kind: wireContentText,
				text: value.Text,
			})
		case message.DataPart:
			content = append(content, wireContent{
				kind: wireContentText,
				text: "\n" + string(value.Value) + "\n",
			})
		case message.ImagePart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
				ledger.reject(
					fields[message.PartImage],
					"model does not accept image input",
				)
				continue
			}
			if role != "user" {
				ledger.reject(
					fields[message.PartImage],
					"deepseek responses accepts images in user messages only",
				)
				continue
			}
			content = append(content, wireContent{
				kind: wireContentImage,
				uri:  deepseekImageValue(value.Source),
			})
		case message.AudioPart:
			ledger.reject(
				fields[message.PartAudio],
				"audio input is not supported by deepseek responses models",
			)
		case message.VideoPart:
			ledger.reject(
				fields[message.PartVideo],
				"video input is not supported by deepseek responses models",
			)
		case message.FilePart:
			ledger.reject(
				fields[message.PartFile],
				"file references are not supported",
			)
		case message.ToolCallPart:
			flush()
			wire.items = append(wire.items, responseWireItem{
				kind:   responseWireItemToolCall,
				callID: value.Call.ID,
				name:   value.Call.Name,
				args:   bytesClone(value.Call.Arguments),
			})
		case message.ToolResultPart:
			ledger.reject(
				fields[message.PartToolResult],
				"tool results ride tool-role messages, not user or assistant turns",
			)
		case message.ReasoningPart:
			flush()
			compileResponsesReasoning(wire, role, value, entry, fields, ledger)
		}
	}
	flush()
}

// compileResponsesReasoning lowers an assistant reasoning trace into a
// reasoning item. DeepSeek accepts plain reasoning text only; the encrypted
// payload convention of OpenAI has no home here.
func compileResponsesReasoning(
	wire *responseWire,
	role string,
	part message.ReasoningPart,
	entry catalogEntry,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	field := fields[message.PartReasoning]
	if role != "assistant" {
		ledger.reject(field, "reasoning parts belong to assistant context")
		return
	}
	if entry.capabilities.Reasoning.Kind == inference.ReasoningNone {
		ledger.drop(field, "model has no reasoning channel")
		return
	}
	if part.Text == "" {
		ledger.drop(
			field,
			"deepseek responses requires plain reasoning text",
		)
		return
	}
	wire.items = append(wire.items, responseWireItem{
		kind:          responseWireItemReasoning,
		reasoningID:   part.ID,
		reasoningText: part.Text,
	})
}

func compileResponsesToolResults(
	wire *responseWire,
	parts []message.Part,
	fields map[message.PartKind]inference.FieldID,
	ledger *ledger,
) {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			ledger.reject(
				fields[part.Kind()],
				"tool-role content carries tool results only",
			)
			continue
		}
		wire.items = append(wire.items, responseWireItem{
			kind:   responseWireItemToolResult,
			callID: result.Result.CallID,
			output: result.Result.Content,
		})
	}
}

func compileResponsesIntent(
	wire *responseWire,
	intent inference.Intent,
	entry catalogEntry,
	ledger *ledger,
) {
	if text := intent.Text; text != nil {
		if format := text.Response; format != nil {
			switch format.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject:
				wire.textFormat = &wireTextFormat{kind: "json_object"}
			case inference.ResponseJSONSchema:
				wire.textFormat = &wireTextFormat{
					kind:   "json_schema",
					name:   format.Name,
					schema: bytesClone(format.Schema),
					strict: true,
				}
			}
		}
		if text.MaxOutputTokens != nil {
			max := int64(*text.MaxOutputTokens)
			wire.maxTokens = &max
		}
	}
	if intent.Image != nil {
		ledger.reject(
			inference.FieldGenerateIntentImage,
			"text models do not generate images",
		)
	}
	if intent.Audio != nil {
		ledger.reject(
			inference.FieldGenerateIntentAudio,
			"text models do not synthesize speech",
		)
	}
	if intent.Video != nil {
		ledger.reject(
			inference.FieldGenerateIntentVideo,
			"deepseek has no video generation surface",
		)
	}
	text := intent.Text
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		wire.tools = append(wire.tools, wireTool{
			name:        definition.Name,
			description: definition.Description,
			parameters:  bytesClone(definition.InputSchema),
		})
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceAuto:
			wire.toolChoice = &wireToolChoice{mode: "auto"}
		case inference.ToolChoiceNone:
			wire.toolChoice = &wireToolChoice{mode: "none"}
		case inference.ToolChoiceRequired:
			wire.toolChoice = &wireToolChoice{mode: "required"}
		case inference.ToolChoiceNamed:
			wire.toolChoice = &wireToolChoice{mode: "named", name: choice.Name}
		}
	}
	wire.temperature = clonePointer(text.Temperature)
	wire.topP = clonePointer(text.TopP)
	if text.ReasoningEnabled != nil {
		switch {
		case entry.capabilities.Reasoning.Kind == inference.ReasoningNone:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking control",
			)
		case entry.capabilities.Reasoning.Kind == inference.ReasoningAlways &&
			!*text.ReasoningEnabled:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model cannot disable thinking",
			)
		case entry.capabilities.Reasoning.Kind == inference.ReasoningToggle &&
			!*text.ReasoningEnabled:
			wire.reasoning = "none"
		}
		// enabled == true is a no-op: DeepSeek thinks by default.
	}
	if text.ReasoningEffort != "" {
		switch {
		case entry.capabilities.Reasoning.Kind == inference.ReasoningNone:
			ledger.reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no thinking control",
			)
		case len(entry.capabilities.Reasoning.EffortMap) == 0:
			// Spec-declared reasoning models without a dial: honor the
			// request for reasoning itself at the documented default depth
			// and report the lost level. Built-in DeepSeek models think by
			// default, but a custom spec model has no such guarantee.
			wire.reasoning = "high"
			ledger.drop(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no effort dial; thinking enabled at default depth",
			)
		default:
			mode, _ := entry.capabilities.Reasoning.ResolveEffort(
				text.ReasoningEffort,
			)
			wire.reasoning = mode
			if mode != string(text.ReasoningEffort) {
				ledger.drop(
					inference.FieldGenerateIntentReasoningEffort,
					fmt.Sprintf(
						"model maps reasoning effort %q to %q",
						text.ReasoningEffort,
						mode,
					),
				)
			}
		}
	}
}

func compileResponsesGenerateOptions(
	wire *responseWire,
	options GenerateOptions,
	entry catalogEntry,
	ledger *ledger,
) {
	if options.WebSearch == nil {
		return
	}
	if !entry.capabilities.HostedWebSearch {
		ledger.reject(
			inference.ExtensionField("web_search").Qualify(options),
			"model does not support hosted web search",
		)
		return
	}
	search := options.WebSearch
	wire.webSearch = &wireWebSearch{
		searchContextSize: search.SearchContextSize,
		allowedDomains:    append([]string(nil), search.AllowedDomains...),
		city:              search.UserLocation.City,
		country:           search.UserLocation.Country,
		region:            search.UserLocation.Region,
		timezone:          search.UserLocation.Timezone,
		externalWebAccess: clonePointer(search.ExternalWebAccess),
		returnTokenBudget: search.ReturnTokenBudget,
		required:          search.ToolChoice != nil && search.ToolChoice.Required,
	}
}

// ---------------------------------------------------------------------------
// Wire → Responses API params.
// ---------------------------------------------------------------------------

func wireToResponseParams(wire responseWire) responses.ResponseNewParams {
	items := make(responses.ResponseInputParam, 0, len(wire.items))
	for _, item := range wire.items {
		switch item.kind {
		case responseWireItemMessage:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRole(item.role),
					Content: responseMessageContent(item.content),
				},
			})
		case responseWireItemToolCall:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID:    item.callID,
					Name:      item.name,
					Arguments: string(item.args),
				},
			})
		case responseWireItemToolResult:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: item.callID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: param.NewOpt(item.output),
					},
				},
			})
		case responseWireItemReasoning:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfReasoning: &responses.ResponseReasoningItemParam{
					ID: item.reasoningID,
					Content: []responses.ResponseReasoningItemContentParam{
						{Text: item.reasoningText},
					},
				},
			})
		}
	}

	params := responses.ResponseNewParams{
		Model: wire.model,
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
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
		params.Text = responseTextFormatParam(wire.textFormat)
	}
	for _, definition := range wire.tools {
		toolParam := responses.FunctionToolParam{
			Name:       definition.name,
			Parameters: responseSchemaMap(definition.parameters),
		}
		if definition.description != "" {
			toolParam.Description = param.NewOpt(definition.description)
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{
			OfFunction: &toolParam,
		})
	}
	if wire.webSearch != nil {
		params.Tools = append(params.Tools, responses.ToolUnionParam{
			OfWebSearch: responseWebSearchToolParam(wire.webSearch),
		})
	}
	if wire.toolChoice != nil {
		params.ToolChoice = responseToolChoiceParam(wire.toolChoice)
	} else if wire.webSearch != nil && wire.webSearch.required {
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired),
		}
	}
	if wire.requestMetadataEnvelope == "metadata" && len(wire.requestMetadata) > 0 {
		params.Metadata = wire.requestMetadata
	}
	return params
}

// responseMetadataOptions returns the per-request options that project
// canonical request metadata onto nonstandard body fields. The standard
// "metadata" envelope is typed by the SDK and is set in
// wireToResponseParams; "client_metadata" has no SDK type, so it rides
// option.WithJSONSet like DeepSeek's other extra fields.
func responseMetadataOptions(wire responseWire) []option.RequestOption {
	if wire.requestMetadataEnvelope == "" ||
		wire.requestMetadataEnvelope == "metadata" ||
		len(wire.requestMetadata) == 0 {
		return nil
	}
	return []option.RequestOption{
		option.WithJSONSet(wire.requestMetadataEnvelope, wire.requestMetadata),
	}
}

func responseWebSearchToolParam(search *wireWebSearch) *responses.WebSearchToolParam {
	tool := &responses.WebSearchToolParam{
		Type: responses.WebSearchToolTypeWebSearch,
	}
	if search.searchContextSize != "" {
		tool.SearchContextSize =
			responses.WebSearchToolSearchContextSize(search.searchContextSize)
	}
	if len(search.allowedDomains) > 0 {
		tool.Filters = responses.WebSearchToolFiltersParam{
			AllowedDomains: append([]string(nil), search.allowedDomains...),
		}
	}
	if search.city != "" || search.country != "" ||
		search.region != "" || search.timezone != "" {
		tool.UserLocation = responses.WebSearchToolUserLocationParam{
			Type:     "approximate",
			City:     param.NewOpt(search.city),
			Country:  param.NewOpt(search.country),
			Region:   param.NewOpt(search.region),
			Timezone: param.NewOpt(search.timezone),
		}
	}
	extra := map[string]any{}
	if search.externalWebAccess != nil {
		extra["external_web_access"] = *search.externalWebAccess
	}
	if search.returnTokenBudget != "" {
		extra["return_token_budget"] = search.returnTokenBudget
	}
	if len(extra) > 0 {
		tool.SetExtraFields(extra)
	}
	return tool
}

func responseMessageContent(
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
					Detail:   responses.ResponseInputImageDetailAuto,
					ImageURL: param.NewOpt(part.uri),
				},
			})
		}
	}
	return responses.EasyInputMessageContentUnionParam{OfInputItemContentList: list}
}

func responseTextFormatParam(format *wireTextFormat) responses.ResponseTextConfigParam {
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
				responseSchemaMap(format.schema),
			),
		}
	}
	return responses.ResponseTextConfigParam{}
}

func responseToolChoiceParam(
	choice *wireToolChoice,
) responses.ResponseNewParamsToolChoiceUnion {
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

func responseSchemaMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{"type": "object"}
	}
	return decoded
}

// ---------------------------------------------------------------------------
// Unary transport and decode.
// ---------------------------------------------------------------------------

func transportResponsesGenerate(
	client openai.Client,
) inference.Transport[responseWire, generateRaw] {
	return func(ctx context.Context, wire responseWire) (generateRaw, error) {
		response, err := client.Responses.New(
			ctx,
			wireToResponseParams(wire),
			responseMetadataOptions(wire)...,
		)
		if err != nil {
			classified := classifyError(err)
			logInferenceCall(ctx, "generate", wire.model, classified, "", "")
			return generateRaw{}, classified
		}
		raw, err := responsesToRaw(response)
		if err != nil {
			logInferenceCall(ctx, "generate", wire.model, err, "", "")
			return generateRaw{}, err
		}
		logInferenceCall(ctx, "generate", wire.model, nil, "", raw.id)
		return raw, nil
	}
}

// responsesToRaw converts the SDK response into the provider-owned raw
// model, rejecting provider failures with classified errors.
func responsesToRaw(response *responses.Response) (generateRaw, error) {
	if response == nil {
		return generateRaw{}, fmt.Errorf("deepseek: empty responses object")
	}
	if response.Status == responses.ResponseStatusFailed {
		return generateRaw{}, classifyResponsesError(
			string(response.Error.Code),
			response.Error.Message,
		)
	}
	raw := generateRaw{id: response.ID}
	for _, item := range response.Output {
		switch item.Type {
		case "reasoning":
			reasoning := item.AsReasoning()
			if text := reasoningItemContent(reasoning); text != "" {
				raw.reasonings = append(raw.reasonings, rawReasoning{
					id:   item.ID,
					text: text,
				})
			}
		case "message":
			for _, content := range item.Content {
				if content.Type != "output_text" {
					continue
				}
				raw.texts = append(raw.texts, content.Text)
				raw.citations = append(raw.citations,
					deepseekCitations(content.Annotations)...)
			}
		case "function_call":
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   item.CallID,
				name: item.Name,
				args: []byte(item.Arguments.OfString),
			})
		case "web_search_call":
			raw.webSearchCalls = append(raw.webSearchCalls,
				deepseekWebSearchCall(item))
		}
	}
	raw.usage = responsesUsage(response.Usage)
	raw.finish = responsesFinish(response, len(raw.toolCalls) > 0)
	return raw, nil
}

func reasoningItemContent(item responses.ResponseReasoningItem) string {
	var builder strings.Builder
	for _, content := range item.Content {
		if content.Type == "reasoning_text" {
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}

func deepseekWebSearchCall(
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

func deepseekCitations(
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

func responsesUsage(usage responses.ResponseUsage) rawUsage {
	raw := rawUsage{
		input:      usage.InputTokens,
		output:     usage.OutputTokens,
		total:      usage.TotalTokens,
		cached:     usage.InputTokensDetails.CachedTokens,
		cacheWrite: usage.InputTokensDetails.CacheWriteTokens,
		reasoning:  usage.OutputTokensDetails.ReasoningTokens,
		present:    true,
	}
	if raw.total == 0 {
		raw.total = raw.input + raw.output
	}
	return raw
}

func responsesFinish(
	response *responses.Response,
	hasToolCalls bool,
) inference.FinishReason {
	if hasToolCalls {
		return inference.FinishToolCalls
	}
	if response.Status == responses.ResponseStatusIncomplete {
		return responsesIncompleteFinish(response.IncompleteDetails.Reason)
	}
	return inference.FinishCompleted
}

func responsesIncompleteFinish(reason string) inference.FinishReason {
	switch reason {
	case "max_output_tokens", "max_tokens":
		return inference.FinishMaxOutput
	case "content_filter":
		return inference.FinishContentFilter
	}
	return inference.FinishCompleted
}

func classifyResponsesError(code, message string) error {
	err := fmt.Errorf("deepseek: response failed: %s %s", code, message)
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

// ---------------------------------------------------------------------------
// Streaming transport.
// ---------------------------------------------------------------------------

// deepseekStream adapts the SDK SSE reader to ProviderStream[streamRaw]. It
// assigns canonical part indices to output items and collapses snapshot-style
// events into deltas. DeepSeek streams reasoning with
// response.reasoning_text.delta / .done (not summary events), so the
// stateful stage handles those natively.
type deepseekStream struct {
	stream *ssestream.Stream[responses.ResponseStreamEventUnion]

	parts    map[int64]*deepseekStreamPart
	nextPart int
	finished bool
	sawTools bool

	webSearchOutput WebSearchOutput
}

type deepseekStreamPart struct {
	index           int
	tool            bool
	reasoning       bool
	id              string
	sawTextDelta    bool
	sawTextSnapshot bool
}

func transportResponsesGenerateStream(
	client openai.Client,
) inference.Transport[responseWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire responseWire,
	) (inference.ProviderStream[streamRaw], error) {
		stream := client.Responses.NewStreaming(
			ctx,
			wireToResponseParams(wire),
			responseMetadataOptions(wire)...,
		)
		if err := stream.Err(); err != nil {
			classified := classifyError(err)
			logInferenceStream(ctx, "generate", wire.model, classified, "")
			return nil, classified
		}
		logInferenceStream(ctx, "generate", wire.model, nil, "")
		return &deepseekStream{
			stream: stream,
			parts:  make(map[int64]*deepseekStreamPart),
		}, nil
	}
}

func (s *deepseekStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return classifyError(s.stream.Close())
}

func (s *deepseekStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for {
		if !s.stream.Next() {
			if err := s.stream.Err(); err != nil {
				classified := classifyError(err)
				logInferenceStream(ctx, "generate", "", classified, "")
				return streamRaw{}, classified
			}
			return streamRaw{}, io.EOF
		}
		raw, keep, err := s.apply(s.stream.Current())
		if err != nil {
			return streamRaw{}, err
		}
		if keep {
			return raw, nil
		}
	}
}

// apply folds one stream event into a streamRaw. keep=false means the event
// was bookkeeping-only and the loop should read on.
func (s *deepseekStream) apply(
	event responses.ResponseStreamEventUnion,
) (streamRaw, bool, error) {
	switch event.Type {
	case "error":
		return streamRaw{}, false, classifyResponsesError(event.Code, event.Message)
	case "response.failed":
		if event.Response.Error.Message != "" {
			return streamRaw{}, false, classifyResponsesError(
				string(event.Response.Error.Code),
				event.Response.Error.Message,
			)
		}
		return streamRaw{}, false, errdefs.NotAvailablef(
			"deepseek: response failed without detail",
		)
	case "response.completed":
		return s.applyTerminal(&event.Response, inference.FinishCompleted)
	case "response.incomplete":
		return s.applyTerminal(
			&event.Response,
			responsesIncompleteFinish(event.Response.IncompleteDetails.Reason),
		)
	case "response.output_item.added", "response.output_item.done":
		switch event.Item.Type {
		case "reasoning":
			part := s.registerPart(event.OutputIndex, false)
			part.reasoning = true
			if event.Item.ID != "" {
				part.id = event.Item.ID
			}
			if event.Type == "response.output_item.added" {
				return streamRaw{}, false, nil
			}
			text := reasoningItemContent(event.Item.AsReasoning())
			raw := streamRaw{
				kind:        streamRawReasoning,
				part:        part.index,
				reasoningID: part.id,
			}
			if !part.sawTextDelta && !part.sawTextSnapshot && text != "" {
				raw.text = text
				part.sawTextSnapshot = true
			}
			if raw.text == "" && raw.reasoningID == "" {
				return streamRaw{}, false, nil
			}
			return raw, true, nil
		case "function_call":
			part := s.registerPart(event.OutputIndex, true)
			raw := streamRaw{
				kind: streamRawToolFragment,
				part: part.index,
				tool: streamRawTool{
					id:   event.Item.CallID,
					name: event.Item.Name,
				},
			}
			if event.Type == "response.output_item.done" &&
				!part.sawTextDelta && !part.sawTextSnapshot &&
				event.Item.Arguments.OfString != "" {
				raw.tool.argsFragment = event.Item.Arguments.OfString
				part.sawTextSnapshot = true
			}
			return raw, true, nil
		case "web_search_call":
			s.addWebSearchCall(deepseekWebSearchCall(event.Item))
			if event.Type == "response.output_item.done" {
				return s.webSearchSnapshot()
			}
			return streamRaw{}, false, nil
		default:
			return streamRaw{}, false, nil
		}
	case "response.reasoning_text.delta":
		part := s.registerPart(event.OutputIndex, false)
		part.reasoning = true
		part.sawTextDelta = true
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind:        streamRawReasoning,
			part:        part.index,
			text:        event.Delta,
			reasoningID: event.ItemID,
		}, true, nil
	case "response.reasoning_text.done":
		part := s.registerPart(event.OutputIndex, false)
		part.reasoning = true
		if event.ItemID != "" {
			part.id = event.ItemID
		}
		if part.sawTextDelta || part.sawTextSnapshot || event.Text == "" {
			return streamRaw{}, false, nil
		}
		part.sawTextSnapshot = true
		return streamRaw{
			kind:        streamRawReasoning,
			part:        part.index,
			text:        event.Text,
			reasoningID: part.id,
		}, true, nil
	case "response.reasoning_summary_text.delta":
		part := s.registerPart(event.OutputIndex, false)
		part.reasoning = true
		part.sawTextDelta = true
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawReasoning,
			part: part.index,
			text: event.Delta,
		}, true, nil
	case "response.output_text.delta":
		part := s.registerPart(event.OutputIndex, false)
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawText,
			part: part.index,
			text: event.Delta,
		}, true, nil
	case "response.function_call_arguments.delta":
		part := s.registerPart(event.OutputIndex, true)
		part.sawTextDelta = true
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{argsFragment: event.Delta},
		}, true, nil
	case "response.function_call_arguments.done":
		part := s.registerPart(event.OutputIndex, true)
		if part.sawTextDelta || part.sawTextSnapshot || event.Arguments == "" {
			return streamRaw{}, false, nil
		}
		part.sawTextSnapshot = true
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{argsFragment: event.Arguments},
		}, true, nil
	case "response.output_text.annotation.added":
		citation, ok := deepseekAnnotationCitation(event.Annotation)
		if !ok {
			return streamRaw{}, false, nil
		}
		s.addCitation(citation)
		return s.webSearchSnapshot()
	}
	return streamRaw{}, false, nil
}

func (s *deepseekStream) applyTerminal(
	response *responses.Response,
	finish inference.FinishReason,
) (streamRaw, bool, error) {
	if s.sawTools {
		finish = inference.FinishToolCalls
	}
	raw, err := s.finishEvent(response, finish)
	return raw, err == nil, err
}

func (s *deepseekStream) registerPart(outputIndex int64, tool bool) *deepseekStreamPart {
	part, ok := s.parts[outputIndex]
	if !ok {
		part = &deepseekStreamPart{index: s.nextPart}
		s.nextPart++
		s.parts[outputIndex] = part
	}
	if tool {
		part.tool = true
		s.sawTools = true
	}
	return part
}

func (s *deepseekStream) finishEvent(
	response *responses.Response,
	finish inference.FinishReason,
) (streamRaw, error) {
	if s.finished {
		return streamRaw{}, errdefs.Internalf(
			"deepseek: stream emitted a duplicate terminal event",
		)
	}
	s.finished = true
	usage := responsesUsage(response.Usage)
	raw := streamRaw{
		kind:       streamRawFinish,
		usage:      &usage,
		finish:     finish,
		responseID: response.ID,
	}
	if len(s.webSearchOutput.Calls) > 0 || len(s.webSearchOutput.Citations) > 0 {
		raw.providerOutputs = inference.ProviderOutputs{
			&WebSearchOutput{
				Calls:     append([]inference.WebSearchCall(nil), s.webSearchOutput.Calls...),
				Citations: append([]inference.Citation(nil), s.webSearchOutput.Citations...),
			},
		}
	}
	return raw, nil
}

func (s *deepseekStream) addWebSearchCall(call inference.WebSearchCall) {
	for i := range s.webSearchOutput.Calls {
		if s.webSearchOutput.Calls[i].ID != call.ID {
			continue
		}
		if call.Status != "" {
			s.webSearchOutput.Calls[i].Status = call.Status
		}
		if call.Action != "" {
			s.webSearchOutput.Calls[i].Action = call.Action
		}
		if len(call.Queries) > 0 {
			s.webSearchOutput.Calls[i].Queries = call.Queries
		}
		if len(call.Sources) > 0 {
			s.webSearchOutput.Calls[i].Sources = call.Sources
		}
		return
	}
	s.webSearchOutput.Calls = append(s.webSearchOutput.Calls, call)
}

func (s *deepseekStream) addCitation(citation inference.Citation) {
	for i := range s.webSearchOutput.Citations {
		existing := s.webSearchOutput.Citations[i]
		if existing.URL != citation.URL {
			continue
		}
		if existing.StartIndex == nil && citation.StartIndex == nil {
			return
		}
		if existing.StartIndex != nil && citation.StartIndex != nil &&
			*existing.StartIndex == *citation.StartIndex {
			return
		}
	}
	s.webSearchOutput.Citations = append(s.webSearchOutput.Citations, citation)
}

func (s *deepseekStream) webSearchSnapshot() (streamRaw, bool, error) {
	if len(s.webSearchOutput.Calls) == 0 && len(s.webSearchOutput.Citations) == 0 {
		return streamRaw{}, false, nil
	}
	return streamRaw{
		kind: streamRawProviderOutput,
		providerOutputs: inference.ProviderOutputs{
			&WebSearchOutput{
				Calls:     append([]inference.WebSearchCall(nil), s.webSearchOutput.Calls...),
				Citations: append([]inference.Citation(nil), s.webSearchOutput.Citations...),
			},
		},
	}, true, nil
}

func deepseekAnnotationCitation(annotation any) (inference.Citation, bool) {
	raw, err := json.Marshal(annotation)
	if err != nil {
		return inference.Citation{}, false
	}
	var union responses.ResponseOutputTextAnnotationUnion
	if err := json.Unmarshal(raw, &union); err != nil {
		return inference.Citation{}, false
	}
	if union.Type != "url_citation" {
		return inference.Citation{}, false
	}
	url := union.AsURLCitation()
	citation := inference.Citation{
		URL:   url.URL,
		Title: url.Title,
	}
	start, end := url.StartIndex, url.EndIndex
	citation.StartIndex = &start
	citation.EndIndex = &end
	return citation, true
}
