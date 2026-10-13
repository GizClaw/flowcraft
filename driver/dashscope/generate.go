package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// Text generation runs on one of the platform's two generation surfaces,
// selected by the model declaration rather than by the model name:
//
//   - text-generation (.../aigc/text-generation/generation), the classic
//     surface: message content is a string, media parts have no channel, a
//     streamed call pairs the X-DashScope-Sse header with
//     parameters.incremental_output, and parameters.result_format must ask
//     for "message" — the decoder reads messages, not the legacy text shape.
//   - multimodal-generation
//     (.../aigc/multimodal-generation/generation), the surface the Qwen3.5+
//     commercial families and the VL/Omni models require even for text
//     input: message content is an array of typed entries (text, image,
//     video, audio), and a streamed call additionally carries
//     parameters.stream: true.
//
// Both surfaces speak one envelope — {model, input.messages, parameters},
// answered as output.choices[0].message — and they stream the same
// event-stream shape: data lines carry the increment, finish_reason is the
// string "null" until the terminal frame, and the terminal frame closes the
// stream. The stream's accounting rides the terminal frame's cumulative
// usage.
const (
	generatePathText       = "/api/v1/services/aigc/text-generation/generation"
	generatePathMultimodal = "/api/v1/services/aigc/multimodal-generation/generation"
)

// generateWire is one compiled text-generation request: the lowered messages,
// the parameters object as far as the compiler filled it, and the surface the
// transport posts both to.
type generateWire struct {
	model  string
	path   string
	stream bool
	// messages is input.messages in wire order. The wire genuinely mixes
	// string content (the text surface) with content arrays (the multimodal
	// surface), and it carries keys some messages omit, so it is built as
	// maps rather than as a struct whose zero values would emit themselves.
	messages []map[string]any
	params   map[string]any
}

// generateRequest renders the payload for the surface the wire selects.
func generateRequest(wire generateWire) map[string]any {
	return map[string]any{
		"model":      wire.model,
		"input":      map[string]any{"messages": wire.messages},
		"parameters": wire.params,
	}
}

// compileGenerate lowers a canonical request onto the native text surfaces.
// One compiler serves both execution shapes — the shape only switches the
// streaming parameters — and the surface is fixed by the declaration.
func compileGenerate(
	declared ModelSpec,
	reasonScope string,
	name string,
) inference.GenerateCompiler[generateWire] {
	params := declared.Generate
	multimodal := params.API == generateAPIMultimodal
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[generateWire], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		if len(request.RequestMetadata) > 0 {
			ledger.Drop(
				inference.FieldGenerateRequestMetadata,
				"the DashScope text surfaces have no arbitrary request metadata channel",
			)
		}
		wire := generateWire{
			model:  wireModel(name, declared),
			path:   generatePathText,
			stream: shape == inference.GenerateExecutionStream,
			params: make(map[string]any),
		}
		if multimodal {
			wire.path = generatePathMultimodal
		} else {
			// The classic surface defaults to the legacy text result; the
			// message shape is what carries tool calls and reasoning.
			wire.params["result_format"] = "message"
		}
		if wire.stream {
			// Incremental frames: each event carries the new fragment, not
			// the text so far. The multimodal surface pairs the flag with
			// its own stream parameter; the platform refuses either one
			// without the sse header, which the transport sets.
			wire.params["incremental_output"] = true
			if multimodal {
				wire.params["stream"] = true
			}
		}

		lower := lowerer{
			declared:        declared,
			multimodal:      multimodal,
			replayReasoning: params.ReplayReasoning,
			reasonScope:     reasonScope,
			ledger:          ledger,
		}
		for _, turn := range request.Context {
			messages, err := lower.turn(turn, contextPartField)
			if err != nil {
				return inference.Compiled[generateWire]{}, err
			}
			wire.messages = append(wire.messages, messages...)
		}
		input, err := lower.turn(request.Input.Message(), inputPartField)
		if err != nil {
			return inference.Compiled[generateWire]{}, err
		}
		wire.messages = append(wire.messages, input...)

		lower.intent(&wire, request.Input.Content.Intent)
		options, other := inference.ExtensionFor[GenerateOptions](request.Extensions)
		lower.options(&wire, options, request.Input.Content.Intent.Text)
		ledger.RejectExtensions("text generation", other)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[generateWire]{Report: report}, ledger.Err()
		}
		return inference.Compiled[generateWire]{Wire: wire, Report: report}, nil
	}
}

// lowerer lowers canonical messages and intent onto one native surface. It
// carries the declaration facts the lowering reads — media acceptance,
// whether the input consumes reasoning traces back — and records every
// decision on the shared ledger.
type lowerer struct {
	declared        ModelSpec
	multimodal      bool
	replayReasoning bool
	reasonScope     string
	ledger          *inference.Ledger
}

// turn lowers one canonical turn. Roles lower as the platform's messages:
// system and user carry content, assistant may carry content, tool calls and
// a replayed trace, and a tool turn fans out one message per result. fields
// is the ledger field resolver of the position the turn came from, so a
// decision lands on the field the runtime activated.
func (l lowerer) turn(
	turn message.Message,
	fields func(message.PartKind) inference.FieldID,
) ([]map[string]any, error) {
	switch turn.Role {
	case message.RoleSystem:
		// The platform's system message carries text only, so non-text
		// parts are rejected here rather than by the surface rules below —
		// the reason names the real constraint.
		textOnly := make([]message.Part, 0, len(turn.Content.Parts))
		for _, part := range turn.Content.Parts {
			if _, ok := part.(message.TextPart); !ok {
				l.ledger.Reject(
					fields(part.Kind()),
					"the platform's system message carries text only",
				)
				continue
			}
			textOnly = append(textOnly, part)
		}
		return l.role(message.RoleSystem, textOnly, fields)
	case message.RoleUser:
		return l.role(message.RoleUser, turn.Content.Parts, fields)
	case message.RoleAssistant:
		return l.assistant(turn.Content.Parts, fields)
	case message.RoleTool:
		return l.tools(turn.Content.Parts, fields)
	default:
		return nil, fmt.Errorf(
			"dashscope: unsupported message role %q", turn.Role)
	}
}

// role lowers a content-carrying turn (system or user).
func (l lowerer) role(
	role message.Role,
	parts []message.Part,
	fields func(message.PartKind) inference.FieldID,
) ([]map[string]any, error) {
	content, err := l.content(parts, fields)
	if err != nil {
		return nil, err
	}
	return []map[string]any{{"role": string(role), "content": content}}, nil
}

// content lowers the parts of one content-carrying turn. The text surface
// joins the text parts into the single string its message carries and rejects
// every other kind; the multimodal surface keeps the parts as typed entries,
// in order.
func (l lowerer) content(
	parts []message.Part,
	fields func(message.PartKind) inference.FieldID,
) (any, error) {
	if l.multimodal {
		items := make([]map[string]any, 0, len(parts))
		for _, part := range parts {
			item, ok := l.contentItem(part, fields)
			if ok {
				items = append(items, item)
			}
		}
		return items, nil
	}
	var text strings.Builder
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(value.Text)
		case message.ImagePart, message.VideoPart, message.AudioPart:
			l.ledger.Reject(
				fields(part.Kind()),
				"the text-generation surface carries text only; declare generate.api: multimodal for media input",
			)
		case message.ToolCallPart:
			l.ledger.Reject(
				fields(part.Kind()),
				"tool calls belong to assistant turns",
			)
		case message.ToolResultPart:
			l.ledger.Reject(
				fields(part.Kind()),
				"tool results belong to a tool-role turn",
			)
		case message.ReasoningPart:
			l.ledger.Reject(
				fields(part.Kind()),
				"reasoning belongs to assistant turns",
			)
		default:
			l.ledger.Reject(
				fields(part.Kind()),
				"the text-generation surface has no channel for this content kind; inline it as text",
			)
		}
	}
	return text.String(), nil
}

// contentItem lowers one part of the multimodal surface's content array: a
// typed entry per part, in order. A part kind the surface cannot carry
// rejects, and the caller drops it from the array.
func (l lowerer) contentItem(
	part message.Part,
	fields func(message.PartKind) inference.FieldID,
) (map[string]any, bool) {
	switch value := part.(type) {
	case message.TextPart:
		return map[string]any{"text": value.Text}, true
	case message.ImagePart:
		if !slices.Contains(l.declared.Capabilities.Inputs, message.PartImage) {
			l.ledger.Reject(
				fields(message.PartImage),
				"model does not accept image input",
			)
			return nil, false
		}
		uri, err := imageSourceValue(value.Source)
		if err != nil {
			l.ledger.Reject(
				fields(message.PartImage),
				fmt.Sprintf("image input is not renderable: %v", err),
			)
			return nil, false
		}
		return map[string]any{"image": uri}, true
	case message.VideoPart:
		if !slices.Contains(l.declared.Capabilities.Inputs, message.PartVideo) {
			l.ledger.Reject(
				fields(message.PartVideo),
				"model does not accept video input",
			)
			return nil, false
		}
		uri, err := videoSourceValue(value.Source)
		if err != nil {
			l.ledger.Reject(
				fields(message.PartVideo),
				fmt.Sprintf("video input is not renderable: %v", err),
			)
			return nil, false
		}
		return map[string]any{"video": uri}, true
	case message.AudioPart:
		if !slices.Contains(l.declared.Capabilities.Inputs, message.PartAudio) {
			l.ledger.Reject(
				fields(message.PartAudio),
				"model does not accept audio input",
			)
			return nil, false
		}
		uri, err := audioSourceValue(value.Source)
		if err != nil {
			l.ledger.Reject(
				fields(message.PartAudio),
				fmt.Sprintf("audio input is not renderable: %v", err),
			)
			return nil, false
		}
		return map[string]any{"audio": uri}, true
	case message.ToolCallPart:
		l.ledger.Reject(
			fields(part.Kind()),
			"tool calls belong to assistant turns",
		)
		return nil, false
	case message.ToolResultPart:
		l.ledger.Reject(
			fields(part.Kind()),
			"tool results belong to a tool-role turn",
		)
		return nil, false
	case message.ReasoningPart:
		l.ledger.Reject(
			fields(part.Kind()),
			"reasoning belongs to assistant turns",
		)
		return nil, false
	default:
		l.ledger.Reject(
			fields(part.Kind()),
			"the multimodal-generation surface has no channel for this content kind; inline it as text",
		)
		return nil, false
	}
}

// assistant lowers one assistant turn: its content parts, its tool calls, and
// the reasoning trace where the declaration replays one. The platform pairs
// the call identifier with each tool call and takes the arguments as a JSON
// string.
func (l lowerer) assistant(
	parts []message.Part,
	fields func(message.PartKind) inference.FieldID,
) ([]map[string]any, error) {
	var (
		contentParts []message.Part
		toolCalls    []map[string]any
		reasoning    []string
	)
	for _, part := range parts {
		switch value := part.(type) {
		case message.ToolCallPart:
			toolCalls = append(toolCalls, map[string]any{
				"id":   value.Call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      value.Call.Name,
					"arguments": string(value.Call.Arguments),
				},
			})
		case message.ReasoningPart:
			l.replayTrace(&reasoning, value, fields)
		default:
			contentParts = append(contentParts, part)
		}
	}
	wire := map[string]any{"role": string(message.RoleAssistant)}
	switch {
	case len(contentParts) > 0:
		content, err := l.content(contentParts, fields)
		if err != nil {
			return nil, err
		}
		wire["content"] = content
	case len(toolCalls) == 0:
		// A turn with neither content nor calls keeps its message well
		// formed; with calls the platform lets content be omitted.
		wire["content"] = ""
	}
	if len(toolCalls) > 0 {
		wire["tool_calls"] = toolCalls
	}
	if len(reasoning) > 0 {
		wire["reasoning_content"] = strings.Join(reasoning, "\n")
	}
	return []map[string]any{wire}, nil
}

// replayReasoning collects the trace text of a reasoning part when the
// declaration consumes one back, and drops it with the reason otherwise: the
// platform rejects a trace it cannot verify, and a trace produced for another
// verification scope can never be verified here.
func (l lowerer) replayTrace(
	collect *[]string,
	part message.ReasoningPart,
	fields func(message.PartKind) inference.FieldID,
) {
	field := fields(message.PartReasoning)
	switch {
	case !l.replayReasoning:
		l.ledger.Drop(
			field,
			"this model's input does not consume reasoning_content; set generate.replay_reasoning where the platform requires the trace back",
		)
	case part.Source != "" && l.reasonScope != "" && part.Source != l.reasonScope:
		l.ledger.Drop(
			field,
			"the trace was produced for another verification scope",
		)
	case part.Text == "":
		l.ledger.Drop(field, "the trace carries no text to replay")
	default:
		*collect = append(*collect, part.Text)
	}
}

// tools lowers one tool-role turn: every tool result becomes its own message,
// because the platform pairs one content string with one tool call id.
func (l lowerer) tools(
	parts []message.Part,
	fields func(message.PartKind) inference.FieldID,
) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			l.ledger.Reject(
				fields(part.Kind()),
				"tool-role content carries tool results only",
			)
			continue
		}
		messages = append(messages, map[string]any{
			"role":         string(message.RoleTool),
			"content":      l.toolResultContent(result.Result, fields(message.PartToolResult)),
			"tool_call_id": result.Result.CallID,
		})
	}
	return messages, nil
}

// toolResultContent lowers a tool result's content into the string the native
// tool message carries. Text and structured data ride; every other kind
// becomes an in-place placeholder reported on the ledger, so the model still
// sees where something was omitted and the loss is auditable instead of
// silent. The native message has no error flag: a failed result rides as
// plain content.
func (l lowerer) toolResultContent(
	result message.ToolResult,
	field inference.FieldID,
) string {
	var builder strings.Builder
	omitted := make([]string, 0, len(result.Content.Parts))
	for _, part := range result.Content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			builder.WriteString(value.Text)
		case message.DataPart:
			builder.WriteString("\n")
			builder.Write(value.Value)
			builder.WriteString("\n")
		default:
			reason := fmt.Sprintf(
				"%s (the platform's tool message carries text only)",
				part.Kind(),
			)
			omitted = append(omitted, reason)
			builder.WriteString("[omitted tool output: " + reason + "]")
		}
	}
	if len(omitted) > 0 {
		l.ledger.Drop(
			field,
			"tool output omitted: "+strings.Join(omitted, ", "),
		)
	}
	return builder.String()
}

// intent lowers the output controls. A non-text modality has no surface here
// — the model publishes text — and each one is rejected on its own field so
// the report names what the caller asked for.
func (l lowerer) intent(wire *generateWire, intent inference.Intent) {
	if intent.Image != nil {
		l.ledger.Reject(
			inference.FieldGenerateIntentImage,
			"this model publishes text output; route a Wan image model",
		)
	}
	if intent.Audio != nil {
		l.ledger.Reject(
			inference.FieldGenerateIntentAudio,
			"the generate surface produces text, not speech",
		)
	}
	if intent.Video != nil {
		l.ledger.Reject(
			inference.FieldGenerateIntentVideo,
			"this model publishes text output; route a Wan video model",
		)
	}
	text := intent.Text
	if text == nil {
		return
	}
	if format := text.Response; format != nil {
		switch format.Kind {
		case inference.ResponseText:
			// The default shape: nothing to send.
		case inference.ResponseJSONObject:
			wire.params["response_format"] = map[string]any{
				"type": "json_object",
			}
		case inference.ResponseJSONSchema:
			wire.params["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   format.Name,
					"schema": json.RawMessage(format.Schema),
					"strict": true,
				},
			}
		}
	}
	if text.MaxOutputTokens != nil {
		wire.params[l.maxTokensParam()] = *text.MaxOutputTokens
	}
	if len(text.Tools) > 0 {
		tools := make([]map[string]any, 0, len(text.Tools))
		for _, definition := range text.Tools {
			function := map[string]any{
				"name":       definition.Name,
				"parameters": json.RawMessage(definition.InputSchema),
			}
			if definition.Description != "" {
				function["description"] = definition.Description
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": function,
			})
		}
		wire.params["tools"] = tools
	}
	if choice := text.ToolChoice; choice != nil {
		switch choice.Kind {
		case inference.ToolChoiceNamed:
			wire.params["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choice.Name},
			}
		default:
			// auto, none, and required ride verbatim.
			wire.params["tool_choice"] = string(choice.Kind)
		}
	}
	if text.Temperature != nil {
		wire.params["temperature"] = *text.Temperature
	}
	if text.TopP != nil {
		wire.params["top_p"] = *text.TopP
	}
	l.reasoning(wire, text)
}

// maxTokensParam resolves which wire parameter the canonical output-token
// control lowers to for the declared model.
func (l lowerer) maxTokensParam() string {
	switch l.declared.Generate.MaxTokensParam {
	case maxTokensParamCompletion:
		return maxTokensParamCompletion
	default:
		return maxTokensParamMaxTokens
	}
}

// reasoning lowers the thinking controls. The platform's dial is binary —
// enable_thinking — plus the token budget that rides the options extension;
// the declared ReasoningKind decides which requests compile. reasoning_effort
// has no level dial to land on, so it is dropped with that reason rather than
// silently quantized.
func (l lowerer) reasoning(wire *generateWire, text *inference.TextIntent) {
	kind := l.declared.Capabilities.Reasoning.Kind
	if text.ReasoningEnabled != nil {
		switch {
		case kind == model.ReasoningNone:
			l.ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking control",
			)
		case kind == model.ReasoningAlways && !*text.ReasoningEnabled:
			l.ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model cannot disable thinking",
			)
		default:
			wire.params["enable_thinking"] = *text.ReasoningEnabled
		}
	}
	if text.ReasoningEffort != "" {
		if kind == model.ReasoningNone {
			l.ledger.Reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no thinking control",
			)
			return
		}
		l.ledger.Drop(
			inference.FieldGenerateIntentReasoningEffort,
			"the DashScope thinking depth dial is a token budget, not a level; set generate_options.thinking_budget to spend one",
		)
	}
}

// options lowers the generate options extension: the sampler dials, the
// server-side search switches, the thinking budget, and the VL
// high-resolution switch. Every knob binds only where the declaration names
// it, so a knob an undeclared surface would silently swallow rejects instead.
func (l lowerer) options(
	wire *generateWire,
	options GenerateOptions,
	text *inference.TextIntent,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
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
		switch {
		case !l.declared.Capabilities.HostedWebSearch:
			l.ledger.Reject(
				field("enable_search"),
				"model does not declare hosted web search",
			)
		default:
			wire.params["enable_search"] = *options.EnableSearch
		}
	}
	if options.SearchOptions != nil {
		switch {
		case !l.declared.Capabilities.HostedWebSearch:
			l.ledger.Reject(
				field("search_options"),
				"model does not declare hosted web search",
			)
		case options.EnableSearch == nil || !*options.EnableSearch:
			l.ledger.Reject(
				field("search_options"),
				"search_options apply only while web search is enabled; set enable_search to true",
			)
		default:
			search := make(map[string]any)
			if strategy := options.SearchOptions.Strategy; strategy != "" {
				search["strategy"] = strategy
			}
			if options.SearchOptions.EnableSource != nil {
				search["enable_source"] = *options.SearchOptions.EnableSource
			}
			if options.SearchOptions.EnableCitation != nil {
				search["enable_citation"] = *options.SearchOptions.EnableCitation
			}
			if format := options.SearchOptions.CitationFormat; format != "" {
				search["citation_format"] = format
			}
			wire.params["search_options"] = search
		}
	}
	if options.ThinkingBudget != nil {
		switch {
		case l.declared.Capabilities.Reasoning.Kind == model.ReasoningNone:
			l.ledger.Reject(
				field("thinking_budget"),
				"model has no thinking control",
			)
		case text != nil && text.ReasoningEnabled != nil && !*text.ReasoningEnabled:
			l.ledger.Reject(
				field("thinking_budget"),
				"thinking is disabled on this request; a thinking budget cannot bind",
			)
		default:
			wire.params["thinking_budget"] = *options.ThinkingBudget
		}
	}
	if options.VlHighResolutionImages != nil {
		if !slices.Contains(l.declared.Capabilities.Inputs, message.PartImage) {
			l.ledger.Reject(
				field("vl_high_resolution_images"),
				"model does not accept image input",
			)
		} else {
			wire.params["vl_high_resolution_images"] =
				*options.VlHighResolutionImages
		}
	}
}

// openGenerate binds the text-generation compiler to the platform's HTTP
// surfaces. Both execution shapes ride the same compiler; the reasoning scope
// is stamped on the traces the model produces so a later turn can tell
// whether this target may replay one.
func openGenerate(
	cls *clients,
	spec Spec,
	declared ModelSpec,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	scope := inference.ReasoningScope(
		spec.ReasoningScope, id.Provider, id.Name, profile)
	return inference.BindGenerateOperations(
		compileGenerate(declared, scope, id.Name),
		transportGenerate(cls.media),
		inference.WithReasoningSource(decodeGenerate, scope),
		transportGenerateStream(cls.media),
		inference.WithReasoningDeltaSource(decodeGenerateStream, scope),
	)
}
