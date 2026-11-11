package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

const (
	generatePathText       = "/api/v1/services/aigc/text-generation/generation"
	generatePathMultimodal = "/api/v1/services/aigc/multimodal-generation/generation"
)

type generateWire struct {
	model    string
	path     string
	stream   bool
	messages []map[string]any
	params   map[string]any
}

func generateRequest(wire generateWire) map[string]any {
	return map[string]any{
		"model":      wire.model,
		"input":      map[string]any{"messages": wire.messages},
		"parameters": wire.params,
	}
}

type generateEnvelope struct {
	Output struct {
		Choices []generateChoice `json:"choices"`
	} `json:"output"`
	Usage     generateUsage `json:"usage"`
	RequestID string        `json:"request_id"`
	Code      string        `json:"code"`
	Message   string        `json:"message"`
}

type generateChoice struct {
	FinishReason string              `json:"finish_reason"`
	Message      generateMessageBody `json:"message"`
}

type generateMessageBody struct {
	Role             string                `json:"role"`
	Content          json.RawMessage       `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	ToolCalls        []generateToolCallRaw `json:"tool_calls"`
}

type generateToolCallRaw struct {
	ID       string `json:"id"`
	Index    *int   `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type generateUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u generateUsage) canonical() usage.Usage {
	out := usage.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if read := min(max(u.PromptTokensDetails.CachedTokens, 0), u.InputTokens); read > 0 {
		out.Input.CacheReadTokens = &read
		uncached := u.InputTokens - read
		out.Input.UncachedTokens = &uncached
	}
	if reasoning := u.OutputTokensDetails.ReasoningTokens; reasoning > 0 {
		out.Details.Output.ReasoningTokens = &reasoning
		out.Details.Output.ReasoningAccounting = usage.ReasoningIncludedInOutput
	}
	return out
}

type generateRaw struct{ body generateEnvelope }

func compileGenerate(declared ModelSpec, name string) protocol.Compiler[inference.GenerateRequest, generateWire] {
	multimodal := declared.Generate.API == generateAPIMultimodal
	return func(_ context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[generateWire], error) {
		if request.Output.Kind() != inference.OutputText {
			return protocol.Compiled[generateWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("dashscope: generate surface produces text"))
		}
		wire := generateWire{
			model: wireModel(name, declared), path: generatePathText,
			stream: shape == protocol.ShapeStream, params: map[string]any{},
		}
		if multimodal {
			wire.path = generatePathMultimodal
		} else {
			wire.params["result_format"] = "message"
		}
		if wire.stream {
			wire.params["incremental_output"] = true
			if multimodal {
				wire.params["stream"] = true
			}
		}
		var warnings []inference.Warning
		for _, turn := range request.Context {
			messages, err := lowerTurn(turn, declared, multimodal, &warnings)
			if err != nil {
				return protocol.Compiled[generateWire]{}, err
			}
			wire.messages = append(wire.messages, messages...)
		}
		messages, err := lowerTurn(request.Input.Message(), declared, multimodal, &warnings)
		if err != nil {
			return protocol.Compiled[generateWire]{}, err
		}
		wire.messages = append(wire.messages, messages...)
		if len(request.Tools.Definitions()) > 0 {
			tools := make([]map[string]any, 0, len(request.Tools.Definitions()))
			for _, definition := range request.Tools.Definitions() {
				function := map[string]any{"name": definition.Name, "parameters": json.RawMessage(definition.InputSchema)}
				if definition.Description != "" {
					function["description"] = definition.Description
				}
				tools = append(tools, map[string]any{"type": "function", "function": function})
			}
			wire.params["tools"] = tools
		}
		if choice := request.Tools.Choice(); choice != nil {
			switch choice.Mode {
			case inference.ToolChoiceFunction:
				wire.params["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
			case inference.ToolChoiceNone:
				wire.params["tool_choice"] = "none"
			case inference.ToolChoiceRequired:
				wire.params["tool_choice"] = "required"
			default:
				wire.params["tool_choice"] = "auto"
			}
		}
		options, err := generateOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[generateWire]{}, err
		}
		applyGenerateOptions(&wire, options, declared)
		return protocol.Compiled[generateWire]{Wire: wire, Fields: request.ActiveFields(), Warnings: warnings}, nil
	}
}

func generateOptions(extensions inference.Extensions) (*GenerateOptions, error) {
	var found *GenerateOptions
	for _, extension := range extensions {
		options, ok := extension.(*GenerateOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("dashscope: extension %s/%s is not understood", extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: duplicate generate options"))
		}
		found = options
	}
	if found != nil {
		if err := found.Validate(); err != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, err)
		}
	}
	return found, nil
}

func applyGenerateOptions(wire *generateWire, options *GenerateOptions, declared ModelSpec) {
	if options == nil {
		return
	}
	if options.Temperature != nil {
		wire.params["temperature"] = *options.Temperature
	}
	if options.TopP != nil {
		wire.params["top_p"] = *options.TopP
	}
	if options.Seed != nil {
		wire.params["seed"] = *options.Seed
	}
	if options.TopK != nil {
		wire.params["top_k"] = *options.TopK
	}
	if options.RepetitionPenalty != nil {
		wire.params["repetition_penalty"] = *options.RepetitionPenalty
	}
	if options.PresencePenalty != nil {
		wire.params["presence_penalty"] = *options.PresencePenalty
	}
	if len(options.Stop) > 0 {
		wire.params["stop"] = options.Stop
	}
	if options.ParallelToolCalls != nil {
		wire.params["parallel_tool_calls"] = *options.ParallelToolCalls
	}
	if options.EnableSearch != nil {
		wire.params["enable_search"] = *options.EnableSearch
	}
	if options.ThinkingBudget != nil {
		wire.params["thinking_budget"] = *options.ThinkingBudget
	}
	if options.VlHighResolutionImages != nil {
		wire.params["vl_high_resolution_images"] = *options.VlHighResolutionImages
	}
	if options.MaxOutputTokens != nil {
		param := maxTokensParamMaxTokens
		if declared.Generate.MaxTokensParam == maxTokensParamCompletion {
			param = maxTokensParamCompletion
		}
		wire.params[param] = *options.MaxOutputTokens
	}
	if options.ResponseFormat != nil {
		switch options.ResponseFormat.Kind {
		case "json_object":
			wire.params["response_format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			wire.params["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": options.ResponseFormat.Name, "schema": json.RawMessage(options.ResponseFormat.Schema), "strict": options.ResponseFormat.Strict,
			}}
		}
	}
}

func lowerTurn(turn message.Message, declared ModelSpec, multimodal bool, warnings *[]inference.Warning) ([]map[string]any, error) {
	switch turn.Role {
	case message.RoleSystem:
		var text strings.Builder
		for _, part := range turn.Content.Parts {
			switch value := part.(type) {
			case message.TextPart:
				text.WriteString(value.Text)
			case message.DataPart:
				text.WriteString(string(value.Value))
			default:
				return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContext, fmt.Errorf("dashscope: system message carries text only"))
			}
		}
		return []map[string]any{{"role": "system", "content": text.String()}}, nil
	case message.RoleUser:
		content, err := lowerContent(turn.Content.Parts, declared, multimodal)
		if err != nil {
			return nil, err
		}
		return []map[string]any{{"role": "user", "content": content}}, nil
	case message.RoleAssistant:
		wire := map[string]any{"role": "assistant"}
		var contentParts []message.Part
		var calls []map[string]any
		var reasoning []string
		for _, part := range turn.Content.Parts {
			switch value := part.(type) {
			case message.ToolCallPart:
				calls = append(calls, map[string]any{"id": value.Call.ID, "type": "function",
					"function": map[string]any{"name": value.Call.Name, "arguments": value.Call.Arguments.String()}})
			case message.ReasoningPart:
				if declared.Generate.ReplayReasoning && value.Text != "" {
					reasoning = append(reasoning, value.Text)
				} else if value.Text != "" {
					*warnings = append(*warnings, inference.Warning{Field: inference.FieldContent, Code: "reasoning_not_replayed", Message: "dashscope: reasoning_content is not replayed by this model"})
				}
			default:
				contentParts = append(contentParts, part)
			}
		}
		if len(contentParts) > 0 {
			content, err := lowerContent(contentParts, declared, multimodal)
			if err != nil {
				return nil, err
			}
			wire["content"] = content
		} else if len(calls) == 0 {
			wire["content"] = ""
		}
		if len(calls) > 0 {
			wire["tool_calls"] = calls
		}
		if len(reasoning) > 0 {
			wire["reasoning_content"] = strings.Join(reasoning, "\n")
		}
		return []map[string]any{wire}, nil
	case message.RoleTool:
		out := make([]map[string]any, 0, len(turn.Content.Parts))
		for _, part := range turn.Content.Parts {
			result, ok := part.(message.ToolResultPart)
			if !ok {
				return nil, inference.NewError(inference.InvalidRequest, inference.FieldContent, fmt.Errorf("dashscope: tool role carries tool results only"))
			}
			var text strings.Builder
			for _, item := range result.Result.Content.Parts {
				switch value := item.(type) {
				case message.TextPart:
					text.WriteString(value.Text)
				case message.DataPart:
					text.WriteString(string(value.Value))
				default:
					text.WriteString("[omitted " + string(item.Kind()) + "]")
				}
			}
			out = append(out, map[string]any{"role": "tool", "content": text.String(), "tool_call_id": result.Result.CallID})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("dashscope: unsupported role %q", turn.Role)
	}
}

func lowerContent(parts []message.Part, declared ModelSpec, multimodal bool) (any, error) {
	if !multimodal {
		var text strings.Builder
		for _, part := range parts {
			switch value := part.(type) {
			case message.TextPart:
				text.WriteString(value.Text)
			case message.DataPart:
				text.WriteString(string(value.Value))
			default:
				return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("dashscope: text surface carries text only, not %s", part.Kind()))
			}
		}
		return text.String(), nil
	}
	items := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			items = append(items, map[string]any{"text": value.Text})
		case message.ImagePart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartImage) {
				return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept image input"))
			}
			uri, err := imageSourceValue(value.Source)
			if err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"image": uri})
		case message.VideoPart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartVideo) {
				return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept video input"))
			}
			uri, err := videoSourceValue(value.Source)
			if err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"video": uri})
		case message.AudioPart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartAudio) {
				return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept audio input"))
			}
			uri, err := audioSourceValue(value.Source)
			if err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"audio": uri})
		default:
			return nil, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: %s has no content channel", part.Kind()))
		}
	}
	return items, nil
}

func transportGenerate(client *mediaClient) protocol.Transport[inference.GenerateRequest, generateWire, generateRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire generateWire) (generateRaw, error) {
		var envelope generateEnvelope
		if err := client.postJSON(ctx, wire.path, false, generateRequest(wire), &envelope); err != nil {
			return generateRaw{}, err
		}
		if envelope.Code != "" {
			return generateRaw{}, errorBody{Code: envelope.Code, Message: envelope.Message, RequestID: envelope.RequestID}.failure("generate", 200)
		}
		if len(envelope.Output.Choices) == 0 {
			return generateRaw{}, fmt.Errorf("dashscope: generate returned no choices")
		}
		return generateRaw{body: envelope}, nil
	}
}

func decodeGenerate(_ context.Context, request inference.GenerateRequest, raw generateRaw) (inference.GenerateResponse, error) {
	if len(raw.body.Output.Choices) != 1 {
		return inference.GenerateResponse{}, fmt.Errorf("dashscope: generate returned %d choices", len(raw.body.Output.Choices))
	}
	choice := raw.body.Output.Choices[0]
	content, err := decodeGenerateContent(choice.Message.Content)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	parts := make([]message.Part, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.ReasoningContent != "" {
		parts = append(parts, message.ReasoningPart{Text: choice.Message.ReasoningContent})
	}
	if content != "" {
		parts = append(parts, message.TextPart{Text: content})
	}
	for _, call := range choice.Message.ToolCalls {
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: message.NewArguments(json.RawMessage(call.Function.Arguments))}})
	}
	return inference.GenerateResponse{
		Message:      message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: parts}},
		FinishReason: generateFinish(choice.FinishReason, len(choice.Message.ToolCalls) > 0),
		Usage:        raw.body.Usage.canonical(),
		Metadata:     inference.Metadata{Model: request.Model.ID, RequestID: raw.body.RequestID},
	}, nil
}

func generateFinish(raw string, tools bool) inference.FinishReason {
	switch raw {
	case "length":
		return inference.FinishLength
	case "tool_calls":
		return inference.FinishToolCalls
	case "content_filter":
		return inference.FinishContentFilter
	case "", "null":
		if tools {
			return inference.FinishToolCalls
		}
		return inference.FinishStop
	default:
		return inference.FinishStop
	}
}

func decodeGenerateContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, item := range items {
		value, ok := item["text"]
		if !ok {
			return "", fmt.Errorf("dashscope: content entry is not text")
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return "", err
		}
		builder.WriteString(text)
	}
	return builder.String(), nil
}

func (d *driver) compileGenerate(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[generateWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[generateWire]{}, err
	}
	return compileGenerate(declared, request.Model.ID.Name)(ctx, request, shape)
}
