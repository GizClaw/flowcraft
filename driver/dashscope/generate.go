package dashscope

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

const (
	generatePathText       = "/api/v1/services/aigc/text-generation/generation"
	generatePathMultimodal = "/api/v1/services/aigc/multimodal-generation/generation"
)

type generateWire struct {
	model    string
	path     string
	stream   bool
	messages []messageBody
	params   generateParams
}

// generateParams is the parameters block. Presence is explicit: the dialect and the
// shape decide which leaves are set, and an unset knob is omitted rather than sent
// as a zero. max_tokens and max_completion_tokens are separate fields because the
// model declaration picks one and the wire must carry only that one.
type generateParams struct {
	ResultFormat        string                  `json:"result_format,omitempty"`
	IncrementalOutput   *bool                   `json:"incremental_output,omitempty"`
	Stream              *bool                   `json:"stream,omitempty"`
	Tools               []generateToolBody      `json:"tools,omitempty"`
	ToolChoice          *generateToolChoiceBody `json:"tool_choice,omitempty"`
	Temperature         *float64                `json:"temperature,omitempty"`
	TopP                *float64                `json:"top_p,omitempty"`
	Seed                *int64                  `json:"seed,omitempty"`
	TopK                *int                    `json:"top_k,omitempty"`
	RepetitionPenalty   *float64                `json:"repetition_penalty,omitempty"`
	PresencePenalty     *float64                `json:"presence_penalty,omitempty"`
	Stop                []string                `json:"stop,omitempty"`
	ParallelToolCalls   *bool                   `json:"parallel_tool_calls,omitempty"`
	EnableSearch        *bool                   `json:"enable_search,omitempty"`
	ThinkingBudget      *int                    `json:"thinking_budget,omitempty"`
	VLHighResImages     *bool                   `json:"vl_high_resolution_images,omitempty"`
	MaxTokens           *int                    `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                    `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *generateResponseFormat `json:"response_format,omitempty"`
}

// generateToolBody is one function tool: the schema rides as raw JSON so the
// provider receives exactly what the deployment declared.
type generateToolBody struct {
	Type     string               `json:"type"`
	Function generateFunctionBody `json:"function"`
}

type generateFunctionBody struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// generateToolChoiceBody is the union the platform accepts: a mode string, or a
// function object naming the call to force.
type generateToolChoiceBody struct {
	mode string
	name string
}

func (c generateToolChoiceBody) MarshalJSON() ([]byte, error) {
	if c.name == "" {
		return json.Marshal(c.mode)
	}
	forced := struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}{Type: "function"}
	forced.Function.Name = c.name
	return json.Marshal(forced)
}

// generateResponseFormat is the union of a plain JSON mode and a named schema.
type generateResponseFormat struct {
	Type       string              `json:"type"`
	JSONSchema *generateJSONSchema `json:"json_schema,omitempty"`
}

type generateJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
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
			stream: shape == protocol.ShapeStream, params: generateParams{},
		}
		if multimodal {
			wire.path = generatePathMultimodal
		} else {
			wire.params.ResultFormat = "message"
		}
		if wire.stream {
			streaming := true
			wire.params.IncrementalOutput = &streaming
			if multimodal {
				wire.params.Stream = &streaming
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
		if definitions := request.Tools.Definitions(); len(definitions) > 0 {
			tools := make([]generateToolBody, 0, len(definitions))
			for _, definition := range definitions {
				tools = append(tools, generateToolBody{
					Type: "function",
					Function: generateFunctionBody{
						Name:        definition.Name,
						Description: definition.Description,
						Parameters:  definition.InputSchema,
					},
				})
			}
			wire.params.Tools = tools
		}
		if choice := request.Tools.Choice(); choice != nil {
			switch choice.Mode {
			case inference.ToolChoiceFunction:
				wire.params.ToolChoice = &generateToolChoiceBody{mode: "function", name: choice.Name}
			case inference.ToolChoiceNone:
				wire.params.ToolChoice = &generateToolChoiceBody{mode: "none"}
			case inference.ToolChoiceRequired:
				wire.params.ToolChoice = &generateToolChoiceBody{mode: "required"}
			default:
				wire.params.ToolChoice = &generateToolChoiceBody{mode: "auto"}
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
		wire.params.Temperature = options.Temperature
	}
	if options.TopP != nil {
		wire.params.TopP = options.TopP
	}
	if options.Seed != nil {
		wire.params.Seed = options.Seed
	}
	if options.TopK != nil {
		wire.params.TopK = options.TopK
	}
	if options.RepetitionPenalty != nil {
		wire.params.RepetitionPenalty = options.RepetitionPenalty
	}
	if options.PresencePenalty != nil {
		wire.params.PresencePenalty = options.PresencePenalty
	}
	if len(options.Stop) > 0 {
		wire.params.Stop = options.Stop
	}
	if options.ParallelToolCalls != nil {
		wire.params.ParallelToolCalls = options.ParallelToolCalls
	}
	if options.EnableSearch != nil {
		wire.params.EnableSearch = options.EnableSearch
	}
	if options.ThinkingBudget != nil {
		wire.params.ThinkingBudget = options.ThinkingBudget
	}
	if options.VlHighResolutionImages != nil {
		wire.params.VLHighResImages = options.VlHighResolutionImages
	}
	if options.MaxOutputTokens != nil {
		// The declaration picks which budget field the endpoint reads; exactly one
		// goes on the wire.
		if declared.Generate.MaxTokensParam == maxTokensParamCompletion {
			wire.params.MaxCompletionTokens = options.MaxOutputTokens
		} else {
			wire.params.MaxTokens = options.MaxOutputTokens
		}
	}
	if options.ResponseFormat != nil {
		switch options.ResponseFormat.Kind {
		case "json_object":
			wire.params.ResponseFormat = &generateResponseFormat{Type: "json_object"}
		case "json_schema":
			wire.params.ResponseFormat = &generateResponseFormat{
				Type: "json_schema",
				JSONSchema: &generateJSONSchema{
					Name:   options.ResponseFormat.Name,
					Schema: options.ResponseFormat.Schema,
					Strict: options.ResponseFormat.Strict,
				},
			}
		}
	}
}

// messageBody is one wire message. Each role gets its own type because the platform
// accepts a different key set per role: a single struct with optional fields would let
// an illegal combination — a tool message carrying tool_calls — compile.
type messageBody interface{ messageRole() string }

type systemMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (systemMessage) messageRole() string { return string(message.RoleSystem) }

type userMessage struct {
	Role    string         `json:"role"`
	Content messageContent `json:"content"`
}

func (userMessage) messageRole() string { return string(message.RoleUser) }

// assistantMessage is a previous answer. Content is a pointer because a turn that
// carries only tool calls sends no content key at all, while one that carries neither
// content nor calls sends an explicit empty string.
type assistantMessage struct {
	Role             string          `json:"role"`
	Content          *messageContent `json:"content,omitempty"`
	ToolCalls        []toolCallBody  `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

func (assistantMessage) messageRole() string { return string(message.RoleAssistant) }

type toolMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

func (toolMessage) messageRole() string { return string(message.RoleTool) }

// messageContent is the union the two surfaces disagree on: the text surface carries a
// bare string and the multimodal one a typed item array. The fields are unexported so
// the two constructors are the only way in.
type messageContent struct {
	text  *string
	items []contentItemBody
}

// textContent is the string form, which is how the text surface speaks.
func textContent(value string) messageContent { return messageContent{text: &value} }

// itemsContent is the multimodal form: one entry per input part.
func itemsContent(items []contentItemBody) messageContent { return messageContent{items: items} }

func (c messageContent) MarshalJSON() ([]byte, error) {
	if c.items != nil {
		return json.Marshal(c.items)
	}
	if c.text != nil {
		return json.Marshal(*c.text)
	}
	return []byte(`""`), nil
}

// contentItemBody is one multimodal input entry. This wire uses a sibling key per kind
// rather than the nested object the OpenAI family uses.
type contentItemBody struct {
	Text  string `json:"text,omitempty"`
	Image string `json:"image,omitempty"`
	Video string `json:"video,omitempty"`
	Audio string `json:"audio,omitempty"`
}

// toolCallBody is one call the model asked for. Arguments is the serialised JSON the
// model produced, and this wire carries it as a string.
type toolCallBody struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function toolCallFunctionBody `json:"function"`
}

type toolCallFunctionBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func lowerTurn(
	turn message.Message,
	declared ModelSpec,
	multimodal bool,
	warnings *[]inference.Warning,
) ([]messageBody, error) {
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
		return []messageBody{systemMessage{Role: string(message.RoleSystem), Content: text.String()}}, nil
	case message.RoleUser:
		content, err := lowerContent(turn.Content.Parts, declared, multimodal)
		if err != nil {
			return nil, err
		}
		return []messageBody{userMessage{Role: string(message.RoleUser), Content: content}}, nil
	case message.RoleAssistant:
		assistant := assistantMessage{Role: string(message.RoleAssistant)}
		var contentParts []message.Part
		var reasoning []string
		for _, part := range turn.Content.Parts {
			switch value := part.(type) {
			case message.ToolCallPart:
				assistant.ToolCalls = append(assistant.ToolCalls, toolCallBody{
					ID:   value.Call.ID,
					Type: "function",
					Function: toolCallFunctionBody{
						Name:      value.Call.Name,
						Arguments: value.Call.Arguments.String(),
					},
				})
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
			assistant.Content = &content
		} else if len(assistant.ToolCalls) == 0 {
			// Neither content nor calls: the turn still has to say something, and the
			// platform expects the key. This is why Content is a pointer rather than
			// omitempty on a plain string — an empty string would vanish.
			empty := textContent("")
			assistant.Content = &empty
		}
		if len(reasoning) > 0 {
			assistant.ReasoningContent = strings.Join(reasoning, "\n")
		}
		return []messageBody{assistant}, nil
	case message.RoleTool:
		out := make([]messageBody, 0, len(turn.Content.Parts))
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
			out = append(out, toolMessage{
				Role:       string(message.RoleTool),
				Content:    text.String(),
				ToolCallID: result.Result.CallID,
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("dashscope: unsupported role %q", turn.Role)
	}
}

func lowerContent(parts []message.Part, declared ModelSpec, multimodal bool) (messageContent, error) {
	if !multimodal {
		var text strings.Builder
		for _, part := range parts {
			switch value := part.(type) {
			case message.TextPart:
				text.WriteString(value.Text)
			case message.DataPart:
				text.WriteString(string(value.Value))
			default:
				return messageContent{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("dashscope: text surface carries text only, not %s", part.Kind()))
			}
		}
		return textContent(text.String()), nil
	}
	items := make([]contentItemBody, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			items = append(items, contentItemBody{Text: value.Text})
		case message.ImagePart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartImage) {
				return messageContent{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept image input"))
			}
			uri, err := imageSourceValue(value.Source)
			if err != nil {
				return messageContent{}, err
			}
			items = append(items, contentItemBody{Image: uri})
		case message.VideoPart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartVideo) {
				return messageContent{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept video input"))
			}
			uri, err := videoSourceValue(value.Source)
			if err != nil {
				return messageContent{}, err
			}
			items = append(items, contentItemBody{Video: uri})
		case message.AudioPart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartAudio) {
				return messageContent{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept audio input"))
			}
			uri, err := audioSourceValue(value.Source)
			if err != nil {
				return messageContent{}, err
			}
			items = append(items, contentItemBody{Audio: uri})
		default:
			return messageContent{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: %s has no content channel", part.Kind()))
		}
	}
	return itemsContent(items), nil
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
