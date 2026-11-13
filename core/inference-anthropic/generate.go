package anthropic

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/goccy/go-json"
)

// DefaultMaxTokens pins the required max_tokens parameter when no extension
// supplies one.
const DefaultMaxTokens = 8192

func (d *driver) compile(
	_ context.Context,
	request inference.GenerateRequest,
	_ protocol.Shape,
) (protocol.Compiled[anthropicgo.MessageNewParams], error) {
	if request.Output.Kind() != inference.OutputText {
		return protocol.Compiled[anthropicgo.MessageNewParams]{}, inference.NewError(
			inference.UnsupportedFeature, inference.FieldOutput,
			fmt.Errorf("anthropic: %q output is not served by the Messages API", request.Output.Kind()),
		)
	}
	options, err := generationOptions(request.Extensions)
	if err != nil {
		return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
	}
	if len(d.settings.Models) > 0 {
		descriptor, ok := findModel(d.settings.Models, request.Model.ID)
		if !ok {
			return protocol.Compiled[anthropicgo.MessageNewParams]{}, inference.NewError(
				inference.UnknownModel, inference.FieldModel,
				fmt.Errorf("anthropic: model %q is not declared", request.Model.ID.Name),
			)
		}
		if err := checkModel(descriptor, request); err != nil {
			return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
		}
	}

	params := anthropicgo.MessageNewParams{
		Model:     anthropicgo.Model(request.Model.ID.Name),
		MaxTokens: DefaultMaxTokens,
	}
	if err := applyGenerationOptions(&params, options); err != nil {
		return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
	}
	var warnings []inference.Warning

	for _, turn := range request.Context {
		switch turn.Role {
		case message.RoleSystem:
			if err := compileSystem(&params, turn.Content.Parts); err != nil {
				return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
			}
		case message.RoleTool:
			if err := compileToolResults(&params, turn.Content.Parts); err != nil {
				return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
			}
		case message.RoleUser, message.RoleAssistant:
			if err := compileMessage(&params, turn.Role, turn.Content.Parts, &warnings); err != nil {
				return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
			}
		default:
			return protocol.Compiled[anthropicgo.MessageNewParams]{}, inference.NewError(
				inference.InvalidRequest, inference.FieldContext,
				fmt.Errorf("anthropic: unknown context role %q", turn.Role),
			)
		}
	}

	input := request.Input.Message()
	if request.Input.Role == inference.InputRoleTool {
		if err := compileToolResults(&params, input.Content.Parts); err != nil {
			return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
		}
	} else if err := compileMessage(&params, message.RoleUser, input.Content.Parts, &warnings); err != nil {
		return protocol.Compiled[anthropicgo.MessageNewParams]{}, err
	}

	if request.Tools.Len() > 0 {
		for _, definition := range request.Tools.Definitions() {
			params.Tools = append(params.Tools, d.toolParam(definition))
		}
	}
	if choice := request.Tools.Choice(); choice != nil {
		params.ToolChoice = toolChoiceParam(*choice)
	}

	return protocol.Compiled[anthropicgo.MessageNewParams]{
		Wire:     params,
		Fields:   request.ActiveFields(),
		Warnings: warnings,
	}, nil
}

func (d *driver) call(
	ctx context.Context,
	_ inference.GenerateRequest,
	params anthropicgo.MessageNewParams,
) (*anthropicgo.Message, error) {
	message, err := d.client.Messages.New(ctx, params)
	if err != nil {
		return nil, classifyError(err)
	}
	return message, nil
}

func (d *driver) decode(
	_ context.Context,
	request inference.GenerateRequest,
	raw *anthropicgo.Message,
) (inference.GenerateResponse, error) {
	if raw == nil {
		return inference.GenerateResponse{}, fmt.Errorf("anthropic: empty message object")
	}
	parts := make([]message.Part, 0, len(raw.Content))
	for _, block := range raw.Content {
		switch block.Type {
		case "text":
			parts = append(parts, message.TextPart{Text: block.Text})
		case "thinking":
			parts = append(parts, message.ReasoningPart{
				Text:      block.Thinking,
				Signature: block.Signature,
				Source:    d.reasoningScope(request),
			})
		case "redacted_thinking":
			parts = append(parts, message.ReasoningPart{
				Signature: block.Data,
				Source:    d.reasoningScope(request),
			})
		case "tool_use":
			parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: message.NewArguments(json.RawMessage(block.Input)),
			}})
		}
	}
	finish, err := finishReason(raw.StopReason, len(parts) > 0)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: finish,
		Usage:        usageOf(raw.Usage),
		Metadata: inference.Metadata{
			Model:      request.Model.ID,
			ResponseID: raw.ID,
		},
	}, nil
}

func (d *driver) reasoningScope(request inference.GenerateRequest) string {
	if d.settings.Wire.ReasoningScope != "" {
		return d.settings.Wire.ReasoningScope
	}
	return ProviderID + ":" + request.Model.ID.Name
}

func compileSystem(params *anthropicgo.MessageNewParams, parts []message.Part) error {
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			params.System = append(params.System, anthropicgo.TextBlockParam{Text: value.Text})
		case message.DataPart:
			params.System = append(params.System, anthropicgo.TextBlockParam{
				Text: "\n" + string(value.Value) + "\n",
			})
		default:
			return inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("anthropic: system blocks carry text only, not %q", part.Kind()),
			)
		}
	}
	return nil
}

func compileMessage(
	params *anthropicgo.MessageNewParams,
	role message.Role,
	parts []message.Part,
	warnings *[]inference.Warning,
) error {
	target := anthropicgo.MessageParamRoleUser
	if role == message.RoleAssistant {
		target = anthropicgo.MessageParamRoleAssistant
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			appendBlock(params, target, textBlock(value.Text))
		case message.DataPart:
			appendBlock(params, target, dataBlock(value.Value))
		case message.ToolCallPart:
			if role != message.RoleAssistant {
				return inference.NewError(
					inference.InvalidRequest, inference.FieldContent,
					errors.New("anthropic: tool calls belong to assistant turns"),
				)
			}
			appendBlock(params, target, toolUseBlock(value.Call.ID, value.Call.Name, value.Call.Arguments.Raw()))
		case message.ToolResultPart:
			return inference.NewError(
				inference.InvalidRequest, inference.FieldContent,
				errors.New("anthropic: tool results belong to the tool role"),
			)
		case message.ReasoningPart:
			if role != message.RoleAssistant {
				return inference.NewError(
					inference.InvalidRequest, inference.FieldContent,
					errors.New("anthropic: reasoning belongs to assistant turns"),
				)
			}
			if value.Signature == "" {
				*warnings = append(*warnings, inference.Warning{
					Field:   inference.FieldContent,
					Code:    "unsigned_reasoning",
					Message: "anthropic: reasoning without a signature cannot round-trip",
				})
				continue
			}
			if value.Text == "" {
				appendBlock(params, target, redactedThinkingBlock(value.Signature))
			} else {
				appendBlock(params, target, thinkingBlock(value.Text, value.Signature))
			}
		case message.ImagePart:
			appendBlock(params, target, imageBlock(value.Source))
		case message.FilePart:
			block, err := documentBlock(value)
			if err != nil {
				return err
			}
			appendBlock(params, target, block)
		case message.AudioPart, message.VideoPart:
			return inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("anthropic: %q input is not served", part.Kind()),
			)
		default:
			return inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("anthropic: unsupported content part %q", part.Kind()),
			)
		}
	}
	return nil
}

func compileToolResults(params *anthropicgo.MessageNewParams, parts []message.Part) error {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			return inference.NewError(
				inference.InvalidRequest, inference.FieldContent,
				errors.New("anthropic: tool-role content carries tool results only"),
			)
		}
		content := make([]anthropicgo.ToolResultBlockParamContentUnion, 0, len(result.Result.Content.Parts))
		for _, inner := range result.Result.Content.Parts {
			switch value := inner.(type) {
			case message.TextPart:
				content = append(content, toolResultText(value.Text))
			case message.DataPart:
				content = append(content, toolResultText("\n"+string(value.Value)+"\n"))
			case message.ImagePart:
				content = append(content, anthropicgo.ToolResultBlockParamContentUnion{
					OfImage: &anthropicgo.ImageBlockParam{Source: imageSource(value.Source)},
				})
			default:
				return inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("anthropic: tool output %q is not served yet", inner.Kind()),
				)
			}
		}
		appendBlock(params, anthropicgo.MessageParamRoleUser,
			toolResultBlock(result.Result.CallID, content, result.Result.IsError))
	}
	return nil
}

func appendBlock(
	params *anthropicgo.MessageNewParams,
	role anthropicgo.MessageParamRole,
	block anthropicgo.ContentBlockParamUnion,
) {
	if last := len(params.Messages) - 1; last >= 0 && params.Messages[last].Role == role {
		message := &params.Messages[last]
		if block.OfThinking != nil || block.OfRedactedThinking != nil {
			message.Content = append([]anthropicgo.ContentBlockParamUnion{block}, message.Content...)
			return
		}
		message.Content = append(message.Content, block)
		return
	}
	params.Messages = append(params.Messages, anthropicgo.MessageParam{
		Role:    role,
		Content: []anthropicgo.ContentBlockParamUnion{block},
	})
}

func textBlock(text string) anthropicgo.ContentBlockParamUnion {
	return anthropicgo.NewTextBlock(text)
}

func dataBlock(value []byte) anthropicgo.ContentBlockParamUnion {
	return anthropicgo.NewTextBlock("\n" + string(value) + "\n")
}

func thinkingBlock(text, signature string) anthropicgo.ContentBlockParamUnion {
	return anthropicgo.NewThinkingBlock(signature, text)
}

func redactedThinkingBlock(signature string) anthropicgo.ContentBlockParamUnion {
	return anthropicgo.NewRedactedThinkingBlock(signature)
}

func toolUseBlock(callID, name string, args []byte) anthropicgo.ContentBlockParamUnion {
	return anthropicgo.NewToolUseBlock(callID, argsValue(args), name)
}

func (d *driver) toolParam(definition message.ToolDefinition) anthropicgo.ToolUnionParam {
	tool := anthropicgo.ToolParam{
		Name:        definition.Name,
		InputSchema: d.toolInputSchema(definition.InputSchema),
	}
	if definition.Description != "" {
		tool.Description = param.NewOpt(definition.Description)
	}
	return anthropicgo.ToolUnionParam{OfTool: &tool}
}

func imageBlock(source media.ImageSource) anthropicgo.ContentBlockParamUnion {
	if source.Kind() == media.SourceURL {
		return anthropicgo.NewImageBlock(anthropicgo.URLImageSourceParam{
			URL: source.URL(),
		})
	}
	return anthropicgo.NewImageBlock(anthropicgo.Base64ImageSourceParam{
		MediaType: anthropicgo.Base64ImageSourceMediaType(source.MediaType()),
		Data:      base64Encode(source.Bytes()),
	})
}

func imageSource(source media.ImageSource) anthropicgo.ImageBlockParamSourceUnion {
	if source.Kind() == media.SourceURL {
		return anthropicgo.ImageBlockParamSourceUnion{
			OfURL: &anthropicgo.URLImageSourceParam{URL: source.URL()},
		}
	}
	return anthropicgo.ImageBlockParamSourceUnion{
		OfBase64: &anthropicgo.Base64ImageSourceParam{
			MediaType: anthropicgo.Base64ImageSourceMediaType(source.MediaType()),
			Data:      base64Encode(source.Bytes()),
		},
	}
}

func documentBlock(part message.FilePart) (anthropicgo.ContentBlockParamUnion, error) {
	switch part.Source.BaseMediaType() {
	case "application/pdf":
		if part.Source.Kind() == media.SourceURL {
			return anthropicgo.NewDocumentBlock(anthropicgo.URLPDFSourceParam{
				URL: part.Source.URL(),
			}), nil
		}
		return anthropicgo.NewDocumentBlock(anthropicgo.Base64PDFSourceParam{
			Data: base64Encode(part.Source.Bytes()),
		}), nil
	case "text/plain":
		if part.Source.Kind() == media.SourceURL {
			return anthropicgo.ContentBlockParamUnion{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				errors.New("anthropic: a text document must carry inline bytes"),
			)
		}
		return anthropicgo.NewDocumentBlock(anthropicgo.PlainTextSourceParam{
			Data: string(part.Source.Bytes()),
		}), nil
	default:
		return anthropicgo.ContentBlockParamUnion{}, inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("anthropic: document type %q is not served", part.Source.MediaType()),
		)
	}
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func toolResultText(text string) anthropicgo.ToolResultBlockParamContentUnion {
	return anthropicgo.ToolResultBlockParamContentUnion{
		OfText: &anthropicgo.TextBlockParam{Text: text},
	}
}

func toolResultBlock(
	callID string,
	content []anthropicgo.ToolResultBlockParamContentUnion,
	isError bool,
) anthropicgo.ContentBlockParamUnion {
	if len(content) <= 1 && (len(content) == 0 || content[0].OfText != nil) {
		text := ""
		if len(content) == 1 {
			text = content[0].OfText.Text
		}
		return anthropicgo.NewToolResultBlock(callID, text, isError)
	}
	return anthropicgo.ContentBlockParamUnion{
		OfToolResult: &anthropicgo.ToolResultBlockParam{
			ToolUseID: callID,
			Content:   content,
			IsError:   anthropicgo.Bool(isError),
		},
	}
}

func toolChoiceParam(choice inference.ToolChoice) anthropicgo.ToolChoiceUnionParam {
	switch choice.Mode {
	case inference.ToolChoiceNone:
		return anthropicgo.ToolChoiceUnionParam{OfNone: &anthropicgo.ToolChoiceNoneParam{}}
	case inference.ToolChoiceRequired:
		return anthropicgo.ToolChoiceUnionParam{OfAny: &anthropicgo.ToolChoiceAnyParam{}}
	case inference.ToolChoiceFunction:
		return anthropicgo.ToolChoiceUnionParam{OfTool: &anthropicgo.ToolChoiceToolParam{Name: choice.Name}}
	default:
		return anthropicgo.ToolChoiceUnionParam{OfAuto: &anthropicgo.ToolChoiceAutoParam{}}
	}
}

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

// toolInputSchema decodes a tool schema into the shape the SDK marshals.
//
// The decode is pure: the same bytes always produce an equal value, so the instance
// keeps the last few in an LRU instead of re-decoding per tool per request — measured
// at 1.5 µs and 46 allocations for one realistic schema, against one map lookup on a
// hit.
func (d *driver) toolInputSchema(raw []byte) anthropicgo.ToolInputSchemaParam {
	if d.schemas != nil {
		if cached, ok := d.schemas.Get(string(raw)); ok {
			return cached
		}
	}
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
	if d.schemas != nil {
		// A driver assembled by hand carries no cache: decoding still answers, it
		// just does not remember.
		d.schemas.Put(string(raw), param)
	}
	return param
}

func schemaMap(raw []byte) map[string]any {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

func finishReason(reason anthropicgo.StopReason, hasToolCalls bool) (inference.FinishReason, error) {
	switch reason {
	case anthropicgo.StopReasonEndTurn, anthropicgo.StopReasonStopSequence, anthropicgo.StopReasonPauseTurn:
		return inference.FinishStop, nil
	case anthropicgo.StopReasonMaxTokens, anthropicgo.StopReasonModelContextWindowExceeded:
		return inference.FinishLength, nil
	case anthropicgo.StopReasonToolUse:
		return inference.FinishToolCalls, nil
	case anthropicgo.StopReasonRefusal:
		return inference.FinishContentFilter, nil
	case "":
		if hasToolCalls {
			return inference.FinishToolCalls, nil
		}
		return inference.FinishStop, nil
	default:
		return "", inference.NewError(
			inference.ProviderResponseUnusable, "",
			fmt.Errorf("anthropic: unknown stop reason %q", reason),
		)
	}
}

func usageOf(raw anthropicgo.Usage) usage.Usage {
	uncached := max(raw.InputTokens, 0)
	read := max(raw.CacheReadInputTokens, 0)
	write := max(raw.CacheCreationInputTokens, 0)
	inputTotal := uncached + read + write
	out := usage.Usage{InputTokens: inputTotal, OutputTokens: raw.OutputTokens}
	if inputTotal > 0 {
		out.Input.UncachedTokens = &uncached
	}
	if read > 0 {
		out.Input.CacheReadTokens = &read
	}
	if write > 0 {
		out.Input.CacheWriteTokens = &write
	}
	writes := map[usage.CacheTTL]int64{}
	if raw.CacheCreation.Ephemeral5mInputTokens > 0 {
		writes[usage.CacheTTL5Minutes] = raw.CacheCreation.Ephemeral5mInputTokens
	}
	if raw.CacheCreation.Ephemeral1hInputTokens > 0 {
		writes[usage.CacheTTL1Hour] = raw.CacheCreation.Ephemeral1hInputTokens
	}
	if len(writes) > 0 {
		out.Details.CacheWrites = writes
	}
	if raw.OutputTokensDetails.ThinkingTokens > 0 {
		thinking := raw.OutputTokensDetails.ThinkingTokens
		out.Details.Output.ReasoningTokens = &thinking
		out.Details.Output.ReasoningAccounting = usage.ReasoningIncludedInOutput
	}
	if raw.ServerToolUse.WebSearchRequests > 0 {
		out.Details.Tools = map[usage.ToolUsageKind]int64{
			usage.ToolUsageWebSearch: raw.ServerToolUse.WebSearchRequests,
		}
	}
	if raw.ServerToolUse.WebFetchRequests > 0 {
		if out.Details.Tools == nil {
			out.Details.Tools = map[usage.ToolUsageKind]int64{}
		}
		out.Details.Tools[usage.ToolUsageWebFetch] = raw.ServerToolUse.WebFetchRequests
	}
	return out
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anthropicgo.Error
	if errors.As(err, &apiErr) {
		kind := inference.ProviderFailure
		switch apiErr.StatusCode {
		case 400, 422:
			kind = inference.InvalidRequest
		case 404:
			kind = inference.UnknownModel
		}
		classified := inference.NewError(kind, "", err)
		classified.RequestID = apiErr.RequestID
		if apiErr.Response != nil {
			classified.RetryAfter = parseRetryAfter(apiErr.Response.Header.Get("Retry-After"))
		}
		return classified
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return inference.NewError(inference.OperationInterrupted, "", err)
	}
	return inference.NewError(inference.ProviderFailure, "", err)
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

// checkModel enforces a declared model's input and output modalities.
func checkModel(descriptor model.ModelDescriptor, request inference.GenerateRequest) error {
	if len(descriptor.Capabilities.Outputs) > 0 {
		text := false
		for _, output := range descriptor.Capabilities.Outputs {
			if output == message.PartText {
				text = true
				break
			}
		}
		if !text {
			return inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				errors.New("anthropic: the model declares no text output"),
			)
		}
	}
	if len(descriptor.Capabilities.Inputs) == 0 {
		return nil
	}
	for _, part := range request.Input.Content.Parts {
		accepted := false
		for _, kind := range descriptor.Capabilities.Inputs {
			if kind == part.Kind() {
				accepted = true
				break
			}
		}
		if !accepted {
			return inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("anthropic: the model does not declare %q input", part.Kind()),
			)
		}
	}
	return nil
}

func findModel(models []model.ModelDescriptor, id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}
