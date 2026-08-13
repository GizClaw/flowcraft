package anthropic

// Anthropic SDK bindings: wire → params, transport, response → raw, decode.
// The wire model stays SDK-free; every union and param wrapper lives here.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// wireToParams converts the provider wire into the SDK's message params.
// The reasoning dialect translates here — it selects which params field
// carries the control, a transport-boundary concern the compiler stays
// out of.
func wireToParams(wire generateWire, control ReasoningControl) anthropicgo.MessageNewParams {
	params := anthropicgo.MessageNewParams{
		Model:     anthropicgo.Model(wire.model),
		MaxTokens: wire.maxTokens,
	}
	for _, line := range wire.system {
		params.System = append(params.System, anthropicgo.TextBlockParam{Text: line})
	}
	for _, message := range wire.messages {
		blocks := make([]anthropicgo.ContentBlockParamUnion, 0, len(message.blocks))
		for _, block := range message.blocks {
			blocks = append(blocks, blockToParam(block))
		}
		if message.role == "assistant" {
			params.Messages = append(params.Messages, anthropicgo.NewAssistantMessage(blocks...))
			continue
		}
		params.Messages = append(params.Messages, anthropicgo.NewUserMessage(blocks...))
	}
	if wire.temperature != nil {
		params.Temperature = param.NewOpt(*wire.temperature)
	}
	if wire.topP != nil {
		params.TopP = param.NewOpt(*wire.topP)
	}
	switch {
	case wire.thinking != nil && !*wire.thinking:
		params.Thinking = anthropicgo.ThinkingConfigParamUnion{
			OfDisabled: &anthropicgo.ThinkingConfigDisabledParam{},
		}
	case wire.effort != "" && control == ReasoningControlEffort:
		params.OutputConfig.Effort = anthropicgo.OutputConfigEffort(wire.effort)
	case wire.effort != "" || (wire.thinking != nil && *wire.thinking):
		// Adaptive covers both the binary-thinking dialect (any effort
		// turned into wire.thinking by the compiler) and an explicit
		// reasoning switch with no level.
		params.Thinking = anthropicgo.ThinkingConfigParamUnion{
			OfAdaptive: &anthropicgo.ThinkingConfigAdaptiveParam{},
		}
	}
	if wire.format != nil {
		params.OutputConfig.Format = anthropicgo.JSONOutputFormatParam{
			Schema: schemaMap(wire.format.schema),
		}
	}
	for _, definition := range wire.tools {
		tool := anthropicgo.ToolParam{
			Name:        definition.name,
			InputSchema: toolInputSchema(definition.schema),
		}
		if definition.description != "" {
			tool.Description = param.NewOpt(definition.description)
		}
		params.Tools = append(params.Tools, anthropicgo.ToolUnionParam{OfTool: &tool})
	}
	if wire.toolChoice != nil {
		params.ToolChoice = toolChoiceParam(*wire.toolChoice)
	}
	return params
}

// blockToParam lowers one wire block into the SDK's content block union.
func blockToParam(block wireBlock) anthropicgo.ContentBlockParamUnion {
	switch block.kind {
	case wireBlockImage:
		if block.imageURL != "" {
			return anthropicgo.NewImageBlock(anthropicgo.URLImageSourceParam{
				URL: block.imageURL,
			})
		}
		return anthropicgo.NewImageBlock(anthropicgo.Base64ImageSourceParam{
			MediaType: anthropicgo.Base64ImageSourceMediaType(block.imageType),
			Data:      base64Encode(block.imageData),
		})
	case wireBlockToolUse:
		return anthropicgo.NewToolUseBlock(block.callID, argsValue(block.args), block.name)
	case wireBlockToolResult:
		return anthropicgo.NewToolResultBlock(block.callID, block.output, false)
	case wireBlockThinking:
		return anthropicgo.NewThinkingBlock(block.signature, block.text)
	case wireBlockRedactedThinking:
		return anthropicgo.NewRedactedThinkingBlock(block.signature)
	default:
		return anthropicgo.NewTextBlock(block.text)
	}
}

// argsValue decodes tool-call arguments for the SDK's any-typed input. An
// empty or malformed payload degrades to an empty object; validity is the
// compiler's contract on the way in.
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

// toolInputSchema lowers a canonical JSON schema into the SDK's typed
// fields: properties and required lift out, the rest rides ExtraFields.
func toolInputSchema(raw []byte) anthropicgo.ToolInputSchemaParam {
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
	for key, value := range decoded {
		switch key {
		case "type", "properties", "required":
		default:
			param.ExtraFields[key] = value
		}
	}
	if len(param.ExtraFields) == 0 {
		param.ExtraFields = nil
	}
	return param
}

func toolChoiceParam(choice wireToolChoice) anthropicgo.ToolChoiceUnionParam {
	switch choice.mode {
	case "none":
		return anthropicgo.ToolChoiceUnionParam{
			OfNone: &anthropicgo.ToolChoiceNoneParam{},
		}
	case "any":
		return anthropicgo.ToolChoiceUnionParam{
			OfAny: &anthropicgo.ToolChoiceAnyParam{},
		}
	case "tool":
		return anthropicgo.ToolChoiceUnionParam{
			OfTool: &anthropicgo.ToolChoiceToolParam{Name: choice.name},
		}
	default:
		return anthropicgo.ToolChoiceUnionParam{
			OfAuto: &anthropicgo.ToolChoiceAutoParam{},
		}
	}
}

// ---------------------------------------------------------------------------
// Transport + response lowering.
// ---------------------------------------------------------------------------

func transportGenerate(
	client anthropicgo.Client,
	control ReasoningControl,
) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		message, err := client.Messages.New(ctx, wireToParams(wire, control))
		if err != nil {
			return generateRaw{}, classifyError(err)
		}
		return messageToRaw(message)
	}
}

// messageToRaw lowers the SDK message into the provider-owned raw model.
// Thinking blocks lower with their signature so the reasoning trace can
// round-trip through later context; redacted blocks carry only the opaque
// data, which the canonical reasoning part keeps in the signature slot.
func messageToRaw(message *anthropicgo.Message) (generateRaw, error) {
	if message == nil {
		return generateRaw{}, fmt.Errorf("anthropic: empty message object")
	}
	raw := generateRaw{id: message.ID}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			raw.texts = append(raw.texts, block.Text)
		case "thinking":
			raw.reasonings = append(raw.reasonings, rawReasoning{
				text:      block.Thinking,
				signature: block.Signature,
			})
		case "redacted_thinking":
			raw.reasonings = append(raw.reasonings, rawReasoning{
				signature: block.Data,
			})
		case "tool_use":
			raw.toolCalls = append(raw.toolCalls, rawToolCall{
				id:   block.ID,
				name: block.Name,
				args: []byte(block.Input),
			})
		}
	}
	raw.usage = rawUsage{
		inputTokens:      message.Usage.InputTokens,
		outputTokens:     message.Usage.OutputTokens,
		cacheReadTokens:  message.Usage.CacheReadInputTokens,
		cacheWriteTokens: message.Usage.CacheCreationInputTokens,
	}
	raw.finish = stopReasonFinish(message.StopReason, len(raw.toolCalls) > 0)
	return raw, nil
}

// stopReasonFinish maps the API's stop reasons onto canonical finish
// reasons. tool_use wins when calls exist; pause_turn completes the turn as
// the API hands control back without an error.
func stopReasonFinish(
	reason anthropicgo.StopReason,
	hasToolCalls bool,
) inference.FinishReason {
	switch reason {
	case anthropicgo.StopReasonMaxTokens,
		anthropicgo.StopReasonModelContextWindowExceeded:
		return inference.FinishMaxOutput
	case anthropicgo.StopReasonToolUse:
		return inference.FinishToolCalls
	case anthropicgo.StopReasonRefusal:
		return inference.FinishRefusal
	default:
		if hasToolCalls {
			return inference.FinishToolCalls
		}
		return inference.FinishCompleted
	}
}

// ---------------------------------------------------------------------------
// Decode — raw → canonical response. Pure.
// ---------------------------------------------------------------------------

func decodeGenerate(
	_ context.Context,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0,
		len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	// The API orders thinking blocks first in its responses; the canonical
	// message keeps that order so context round-trips stay valid.
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{
			Text:      reasoning.text,
			Signature: reasoning.signature,
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
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Usage:        rawUsageCanonical(raw.usage),
		Metadata:     inference.Metadata{ResponseID: raw.id},
	}, nil
}

func rawUsageCanonical(raw rawUsage) inference.Usage {
	usage := inference.Usage{
		InputTokens:  raw.inputTokens,
		OutputTokens: raw.outputTokens,
		TotalTokens:  raw.inputTokens + raw.outputTokens,
	}
	if raw.cacheReadTokens > 0 {
		read := raw.cacheReadTokens
		usage.Input.CacheReadTokens = &read
	}
	if raw.cacheWriteTokens > 0 {
		write := raw.cacheWriteTokens
		usage.Input.CacheWriteTokens = &write
	}
	return usage
}
