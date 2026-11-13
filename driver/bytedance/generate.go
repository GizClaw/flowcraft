package bytedance

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/goccy/go-json"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

type generateRaw struct {
	id         string
	reasonings []rawReasoning
	texts      []string
	toolCalls  []rawToolCall
	finish     inference.FinishReason
	usage      rawUsage
	failed     string
}

type rawReasoning struct {
	id   string
	text string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	inputTokens       int64
	outputTokens      int64
	cachedTokens      int64
	reasoningTokens   int64
	inputAudioTokens  int64
	webSearchRequests int64
	mcpRequests       int64
}

type streamRaw struct {
	kind       streamRawKind
	part       int
	text       string
	id         string
	responseID string
	tool       streamRawTool
	usage      *rawUsage
	finish     inference.FinishReason
}

type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawToolFragment
	streamRawReasoning
	streamRawFinish
)

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

func sourceURI(source media.ImageSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func videoSourceURI(source media.VideoSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func audioSourceURI(source media.AudioSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func compileGenerate(
	endpoint string,
	descriptor model.ModelDescriptor,
) protocol.Compiler[inference.GenerateRequest, *arkresponses.ResponsesRequest] {
	return func(
		_ context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[*arkresponses.ResponsesRequest], error) {
		if request.Output.Kind() != inference.OutputText {
			return protocol.Compiled[*arkresponses.ResponsesRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("bytedance: only text generation uses the Responses API"),
			)
		}
		options, err := generateOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[*arkresponses.ResponsesRequest]{}, err
		}
		ark := &arkresponses.ResponsesRequest{Model: endpoint}
		if shape == protocol.ShapeStream {
			stream := true
			ark.Stream = &stream
		}
		var warnings []inference.Warning
		var system []string
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				for _, part := range turn.Content.Parts {
					switch value := part.(type) {
					case message.TextPart:
						system = append(system, value.Text)
					case message.DataPart:
						system = append(system, "\n"+string(value.Value)+"\n")
					default:
						return protocol.Compiled[*arkresponses.ResponsesRequest]{}, inference.NewError(
							inference.UnsupportedFeature, inference.FieldContext,
							fmt.Errorf("bytedance: system messages carry text only, not %s", part.Kind()),
						)
					}
				}
			case message.RoleTool:
				if err := compileToolResults(ark, turn.Content.Parts); err != nil {
					return protocol.Compiled[*arkresponses.ResponsesRequest]{}, err
				}
			default:
				if err := compileMessage(ark, string(turn.Role), turn.Content.Parts, descriptor, &warnings); err != nil {
					return protocol.Compiled[*arkresponses.ResponsesRequest]{}, err
				}
			}
		}
		if instructions := strings.Join(system, "\n\n"); instructions != "" {
			ark.Instructions = &instructions
		}
		if request.Input.Role == inference.InputRoleTool {
			if err := compileToolResults(ark, request.Input.Content.Parts); err != nil {
				return protocol.Compiled[*arkresponses.ResponsesRequest]{}, err
			}
		} else if err := compileMessage(ark, "user", request.Input.Content.Parts, descriptor, &warnings); err != nil {
			return protocol.Compiled[*arkresponses.ResponsesRequest]{}, err
		}

		for _, definition := range request.Tools.Definitions() {
			ark.Tools = append(ark.Tools, arkTool(definition))
		}
		if choice := request.Tools.Choice(); choice != nil {
			ark.ToolChoice = arkToolChoice(*choice)
		}
		applyGenerateOptions(ark, options)
		return protocol.Compiled[*arkresponses.ResponsesRequest]{
			Wire:     ark,
			Fields:   request.ActiveFields(),
			Warnings: warnings,
		}, nil
	}
}

func generateOptions(extensions inference.Extensions) (*GenerateOptions, error) {
	var found *GenerateOptions
	for _, extension := range extensions {
		options, ok := extension.(*GenerateOptions)
		if !ok {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: extension %s/%s is not understood",
					extension.ProviderID(), extension.ExtensionID()),
			)
		}
		if found != nil {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duplicate generation options"),
			)
		}
		found = options
	}
	if found != nil {
		if err := found.Validate(); err != nil {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions, err,
			)
		}
	}
	return found, nil
}

func applyGenerateOptions(ark *arkresponses.ResponsesRequest, options *GenerateOptions) {
	if options == nil {
		return
	}
	if options.ServiceTier != "" {
		ark.ServiceTier = arkServiceTier(options.ServiceTier)
	}
	if options.Caching != nil {
		cacheType := arkresponses.CacheType_disabled
		if options.Caching.Enabled {
			cacheType = arkresponses.CacheType_enabled
		}
		prefix := options.Caching.Prefix
		ark.Caching = &arkresponses.ResponsesCaching{
			Type:   cacheType.Enum(),
			Prefix: &prefix,
		}
	}
	if options.Store != nil {
		ark.Store = options.Store
	}
	if options.PreviousResponseID != "" {
		previous := options.PreviousResponseID
		ark.PreviousResponseId = &previous
	}
	if options.ExpireAt != nil {
		ark.ExpireAt = options.ExpireAt
	}
	if options.ParallelToolCalls != nil {
		ark.ParallelToolCalls = options.ParallelToolCalls
	}
	if options.MaxToolCalls != nil {
		ark.MaxToolCalls = options.MaxToolCalls
	}
	if options.PromptCacheKey != "" {
		cacheKey := options.PromptCacheKey
		ark.PromptCacheKey = &cacheKey
	}
	if options.SafetyIdentifier != "" {
		identifier := options.SafetyIdentifier
		ark.SafetyIdentifier = &identifier
	}
	if options.WebSearch != nil {
		ark.Tools = append(ark.Tools, arkWebSearchTool(options.WebSearch))
	}
	if options.Temperature != nil {
		ark.Temperature = options.Temperature
	}
	if options.TopP != nil {
		ark.TopP = options.TopP
	}
	if options.MaxOutputTokens != nil {
		max := *options.MaxOutputTokens
		ark.MaxOutputTokens = &max
	}
	if options.ResponseFormat != nil {
		ark.Text = &arkresponses.ResponsesText{
			Format: arkTextFormat(
				options.ResponseFormat.Kind,
				options.ResponseFormat.Name,
				options.ResponseFormat.Schema,
				options.ResponseFormat.Strict,
			),
		}
	}
	setArkThinking(ark, options.ReasoningEnabled, options.ReasoningEffort)
}

func compileMessage(
	ark *arkresponses.ResponsesRequest,
	role string,
	parts []message.Part,
	descriptor model.ModelDescriptor,
	warnings *[]inference.Warning,
) error {
	var content []*arkresponses.ContentItem
	flush := func() {
		if len(content) > 0 {
			appendInputItem(ark, arkMessageItem(role, content))
			content = nil
		}
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			content = append(content, arkContentText(value.Text))
		case message.ImagePart:
			if !slices.Contains(descriptor.Capabilities.Inputs, message.PartImage) {
				return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: model does not accept image input"))
			}
			content = append(content, arkContentImage(sourceURI(value.Source)))
		case message.VideoPart:
			if !slices.Contains(descriptor.Capabilities.Inputs, message.PartVideo) {
				return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: model does not accept video input"))
			}
			content = append(content, arkContentVideo(videoSourceURI(value.Source)))
		case message.AudioPart:
			if !slices.Contains(descriptor.Capabilities.Inputs, message.PartAudio) {
				return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: model does not accept audio input"))
			}
			content = append(content, arkContentAudio(audioSourceURI(value.Source)))
		case message.FilePart:
			return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("bytedance: file references are not supported"))
		case message.DataPart:
			content = append(content, arkContentText("\n"+string(value.Value)+"\n"))
		case message.ToolCallPart:
			flush()
			appendInputItem(ark, arkToolCallItem(value.Call.ID, value.Call.Name, value.Call.Arguments.Raw()))
		case message.ToolResultPart:
			flush()
			appendInputItem(ark, arkToolResultItem(value.Result.CallID,
				compileToolResultContent(value.Result.Content)))
		case message.ReasoningPart:
			flush()
			if role != "assistant" {
				return inference.NewError(inference.InvalidRequest, inference.FieldContent,
					fmt.Errorf("bytedance: reasoning belongs to assistant context"))
			}
			*warnings = append(*warnings, inference.Warning{
				Field:   inference.FieldContent,
				Code:    "reasoning_not_consumed",
				Message: "bytedance: Ark does not consume reasoning input",
			})
			continue
		default:
			return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("bytedance: unsupported content part %q", part.Kind()))
		}
	}
	flush()
	return nil
}

func compileToolResults(ark *arkresponses.ResponsesRequest, parts []message.Part) error {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			return inference.NewError(inference.InvalidRequest, inference.FieldContent,
				fmt.Errorf("bytedance: tool-role content carries tool results only"))
		}
		appendInputItem(ark, arkToolResultItem(result.Result.CallID,
			compileToolResultContent(result.Result.Content)))
	}
	return nil
}

func compileToolResultContent(content message.Content) string {
	var builder strings.Builder
	for _, part := range content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			builder.WriteString(value.Text)
		case message.DataPart:
			builder.WriteString("\n")
			builder.Write(value.Value)
			builder.WriteString("\n")
		default:
			builder.WriteString("[omitted tool output: " + string(part.Kind()) + "]")
		}
	}
	return builder.String()
}

func decodeGenerate(
	_ context.Context,
	request inference.GenerateRequest,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	if raw.failed != "" {
		return inference.GenerateResponse{}, fmt.Errorf("bytedance: response failed: %s", raw.failed)
	}
	parts := make([]message.Part, 0, len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{Text: reasoning.text, ID: reasoning.id})
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
			Arguments: message.NewArguments(arguments),
		}})
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Usage:        rawUsageCanonical(raw.usage),
		Metadata:     inference.Metadata{Model: request.Model.ID, ResponseID: raw.id},
	}, nil
}

func rawUsageCanonical(raw rawUsage) usage.Usage {
	out := usage.Usage{
		InputTokens:  raw.inputTokens,
		OutputTokens: raw.outputTokens,
	}
	if raw.inputTokens > 0 {
		read := min(max(raw.cachedTokens, 0), raw.inputTokens)
		if read > 0 {
			out.Input.CacheReadTokens = &read
		}
		uncached := raw.inputTokens - read
		out.Input.UncachedTokens = &uncached
	}
	if raw.reasoningTokens > 0 {
		reasoning := raw.reasoningTokens
		out.Details.Output.ReasoningTokens = &reasoning
		out.Details.Output.ReasoningAccounting = usage.ReasoningIncludedInOutput
	}
	if raw.inputAudioTokens > 0 {
		out.Details.InputModalities = map[usage.Modality]int64{
			usage.ModalityAudio: raw.inputAudioTokens,
		}
	}
	if raw.webSearchRequests > 0 || raw.mcpRequests > 0 {
		out.Details.Tools = map[usage.ToolUsageKind]int64{}
		if raw.webSearchRequests > 0 {
			out.Details.Tools[usage.ToolUsageWebSearch] = raw.webSearchRequests
		}
		if raw.mcpRequests > 0 {
			out.Details.Tools[usage.ToolUsageMCP] = raw.mcpRequests
		}
	}
	return out
}
