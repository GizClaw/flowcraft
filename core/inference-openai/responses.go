package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Which canonical content kinds each Responses role can carry. This surface
// expresses a tool call and its result as their own items rather than as fields of
// a message, and it has no audio input, so its admissible set differs from chat.
var (
	responsesTextKinds      = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true}
	responsesUserKinds      = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true, message.PartImage: true}
	responsesAssistantKinds = map[message.PartKind]bool{message.PartText: true, message.PartReasoning: true, message.PartToolCall: true}
)

// responsesCompile maps a canonical request onto Responses API parameters.
func (d *driver) responsesCompile(
	_ context.Context,
	request inference.GenerateRequest,
	_ protocol.Shape,
) (protocol.Compiled[responses.ResponseNewParams], error) {
	if err := d.preflight(request); err != nil {
		return protocol.Compiled[responses.ResponseNewParams]{}, err
	}
	options, err := generationOptions(request.Extensions)
	if err != nil {
		return protocol.Compiled[responses.ResponseNewParams]{}, err
	}
	params, err := responsesParams(request, options)
	if err != nil {
		return protocol.Compiled[responses.ResponseNewParams]{}, err
	}
	fields := append(honoredFields(request), extensionFields(options)...)
	return protocol.Compiled[responses.ResponseNewParams]{
		Wire:     params,
		Fields:   fields,
		Warnings: reasoningWarnings(request),
	}, nil
}

func responsesParams(
	request inference.GenerateRequest,
	options *GenerationOptions,
) (responses.ResponseNewParams, error) {
	input, err := responsesInput(request)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(request.Model.ID.Name),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input},
	}
	tools, err := responsesTools(request.Tools.Definitions())
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if choice := request.Tools.Choice(); choice != nil {
		params.ToolChoice = responsesToolChoice(*choice)
	}
	if err := responsesApply(&params, options); err != nil {
		return responses.ResponseNewParams{}, err
	}
	return params, nil
}

// responsesInput walks the canonical conversation into Responses input items.
func responsesInput(request inference.GenerateRequest) ([]responses.ResponseInputItemUnionParam, error) {
	out := make([]responses.ResponseInputItemUnionParam, 0, len(request.Context)+1)
	for i, item := range request.Context {
		mapped, err := responsesItems(item)
		if err != nil {
			return nil, fmt.Errorf("context message %d: %w", i, err)
		}
		out = append(out, mapped...)
	}
	mapped, err := responsesItems(request.Input.Message())
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	return append(out, mapped...), nil
}

func responsesItems(item message.Message) ([]responses.ResponseInputItemUnionParam, error) {
	switch item.Role {
	case message.RoleSystem, message.RoleUser, message.RoleAssistant:
		kinds := responsesTextKinds
		switch item.Role {
		case message.RoleUser:
			kinds = responsesUserKinds
		case message.RoleAssistant:
			kinds = responsesAssistantKinds
		}
		if err := rejectUnsupportedParts(item, kinds); err != nil {
			return nil, err
		}
		role, err := responsesRole(item.Role)
		if err != nil {
			return nil, err
		}
		var out []responses.ResponseInputItemUnionParam
		content, hasMedia, err := responsesContent(item)
		if err != nil {
			return nil, err
		}
		if hasMedia {
			out = append(out, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: role,
					Content: responses.EasyInputMessageContentUnionParam{
						OfInputItemContentList: content,
					},
				},
			})
		} else if text := textOf(item); text != "" {
			out = append(out, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: role,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(text),
					},
				},
			})
		}
		for _, call := range item.ToolCalls() {
			out = append(out, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID:    call.ID,
					Name:      call.Name,
					Arguments: call.Arguments.String(),
				},
			})
		}
		return out, nil

	case message.RoleTool:
		var out []responses.ResponseInputItemUnionParam
		for _, part := range item.Content.Parts {
			result, ok := part.(message.ToolResultPart)
			if !ok {
				return nil, unsupportedPart(part.Kind())
			}
			out = append(out, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: openai.String(result.Result.CallID),
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(result.Result.Content.Text()),
					},
				},
			})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("openai: tool message carries no tool result")
		}
		return out, nil

	default:
		return nil, fmt.Errorf("openai: role %q is not supported", item.Role)
	}
}

// responsesContent builds the content list for a turn carrying media.
func responsesContent(item message.Message) ([]responses.ResponseInputContentUnionParam, bool, error) {
	var out []responses.ResponseInputContentUnionParam
	hasMedia := false
	for _, part := range item.Content.Parts {
		switch typed := part.(type) {
		case message.TextPart:
			out = append(out, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: typed.Text},
			})
		case message.ImagePart:
			url, err := imageURL(typed.Source)
			if err != nil {
				return nil, false, err
			}
			hasMedia = true
			out = append(out, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: openai.String(url),
					Detail:   responses.ResponseInputImageDetailAuto,
				},
			})
		}
	}
	return out, hasMedia, nil
}

func responsesRole(role message.Role) (responses.EasyInputMessageRole, error) {
	switch role {
	case message.RoleSystem:
		return responses.EasyInputMessageRoleSystem, nil
	case message.RoleUser:
		return responses.EasyInputMessageRoleUser, nil
	case message.RoleAssistant:
		return responses.EasyInputMessageRoleAssistant, nil
	default:
		return "", fmt.Errorf("openai: role %q is not supported", role)
	}
}

func responsesTools(definitions []message.ToolDefinition) ([]responses.ToolUnionParam, error) {
	if len(definitions) == 0 {
		return nil, nil
	}
	out := make([]responses.ToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		parameters, err := schemaObject(definition)
		if err != nil {
			return nil, err
		}
		// Strict is deliberately not forced: strict mode demands a schema that
		// lists every property as required and forbids additional ones, so
		// switching it on for every tool would reject perfectly valid schemas.
		// A deployment that wants it can express it once there is a need.
		out = append(out, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        definition.Name,
				Description: openai.String(definition.Description),
				Parameters:  parameters,
			},
		})
	}
	return out, nil
}

func responsesToolChoice(choice inference.ToolChoice) responses.ResponseNewParamsToolChoiceUnion {
	if choice.Mode == inference.ToolChoiceFunction {
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice.Name},
		}
	}
	return responses.ResponseNewParamsToolChoiceUnion{
		OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptions(choice.Mode)),
	}
}

// responsesCall performs the provider I/O.
func (d *driver) responsesCall(
	ctx context.Context,
	request inference.GenerateRequest,
	params responses.ResponseNewParams,
) (*responses.Response, error) {
	extra, err := extensionJSONSet(request.Extensions)
	if err != nil {
		return nil, err
	}
	opts, err := bodyOptions(params, extra)
	if err != nil {
		return nil, err
	}
	return d.client.Responses.New(ctx, params, opts...)
}

// responsesDecode maps the provider's answer onto the canonical response.
func (d *driver) responsesDecode(
	_ context.Context,
	_ inference.GenerateRequest,
	raw *responses.Response,
) (inference.GenerateResponse, error) {
	if raw == nil {
		return inference.GenerateResponse{}, fmt.Errorf("openai: nil response")
	}

	parts := make([]message.Part, 0, 1+len(raw.Output))
	if text := raw.OutputText(); text != "" {
		parts = append(parts, message.TextPart{Text: text})
	}
	toolCalls := 0
	for _, item := range raw.Output {
		if item.Type != "function_call" {
			continue
		}
		toolCalls++
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
			ID:        item.CallID,
			Name:      item.Name,
			Arguments: message.NewArguments(toolArguments(item.Arguments.OfString)),
		}})
	}
	finish, err := responsesFinishReason(string(raw.Status), toolCalls)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: finish,
		Usage:        responsesUsage(raw),
		Metadata: inference.Metadata{
			Model:      model.ModelID{Provider: ProviderID, Name: string(raw.Model)},
			ResponseID: raw.ID,
		},
	}, nil
}

func responsesFinishReason(status string, toolCalls int) (inference.FinishReason, error) {
	switch status {
	case "completed":
		if toolCalls > 0 {
			return inference.FinishToolCalls, nil
		}
		return inference.FinishStop, nil
	case "incomplete":
		return inference.FinishLength, nil
	case "failed", "cancelled":
		return inference.FinishError, nil
	default:
		return "", inference.NewError(
			inference.ProviderResponseUnusable,
			"",
			fmt.Errorf("openai: unknown response status %q", status),
		)
	}
}

func responsesUsage(raw *responses.Response) usage.Usage {
	out := usage.Usage{
		InputTokens:  raw.Usage.InputTokens,
		OutputTokens: raw.Usage.OutputTokens,
		TotalTokens:  raw.Usage.TotalTokens,
	}
	if cached := raw.Usage.InputTokensDetails.CachedTokens; cached > 0 {
		out.Input.CacheReadTokens = &cached
	}
	return out
}

// responsesApply maps per-call options onto Responses parameters.
func responsesApply(params *responses.ResponseNewParams, options *GenerationOptions) error {
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
		params.MaxOutputTokens = openai.Int(int64(*options.MaxOutputTokens))
	}
	if options.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(options.ReasoningEffort),
		}
	}
	if options.ServiceTier != "" {
		params.ServiceTier = responses.ResponseNewParamsServiceTier(options.ServiceTier)
	}
	if options.MaxToolCalls != nil {
		params.MaxToolCalls = openai.Int(int64(*options.MaxToolCalls))
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
	if options.ResponseFormat != nil {
		format, err := responsesResponseFormat(*options.ResponseFormat)
		if err != nil {
			return err
		}
		params.Text.Format = format
	}
	if options.Verbosity != "" {
		params.Text.Verbosity = responses.ResponseTextConfigVerbosity(options.Verbosity)
	}
	if options.Store != nil {
		params.Store = openai.Bool(*options.Store)
	}
	if options.WebSearch != nil {
		tool, err := webSearchTool(*options.WebSearch)
		if err != nil {
			return err
		}
		params.Tools = append(params.Tools, tool)
	}
	return nil
}

// responsesResponseFormat maps the canonical format onto the Responses union.
func responsesResponseFormat(format ResponseFormat) (responses.ResponseFormatTextConfigUnionParam, error) {
	switch format.Kind {
	case FormatJSONObject:
		return responses.ResponseFormatTextConfigUnionParam{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}, nil
	case FormatJSONSchema:
		var schema map[string]any
		if err := json.Unmarshal(format.Schema, &schema); err != nil {
			return responses.ResponseFormatTextConfigUnionParam{}, fmt.Errorf(
				"openai: response schema: %w", err,
			)
		}
		return responses.ResponseFormatTextConfigUnionParam{
			OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
				Name:   format.Name,
				Schema: schema,
				Strict: openai.Bool(format.Strict),
			},
		}, nil
	default:
		return responses.ResponseFormatTextConfigUnionParam{
			OfText: &shared.ResponseFormatTextParam{},
		}, nil
	}
}

// webSearchTool builds the hosted search tool the Responses surface accepts.
func webSearchTool(options WebSearch) (responses.ToolUnionParam, error) {
	tool := &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}
	switch options.ContextSize {
	case "":
	case "low":
		tool.SearchContextSize = responses.WebSearchToolSearchContextSizeLow
	case "medium":
		tool.SearchContextSize = responses.WebSearchToolSearchContextSizeMedium
	case "high":
		tool.SearchContextSize = responses.WebSearchToolSearchContextSizeHigh
	default:
		return responses.ToolUnionParam{}, unsupportedOption(
			"web_search.context_size="+options.ContextSize, "responses",
		)
	}
	if len(options.AllowedDomains) > 0 {
		tool.Filters.AllowedDomains = append([]string(nil), options.AllowedDomains...)
	}
	return responses.ToolUnionParam{OfWebSearch: tool}, nil
}
