package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// DefaultMaxTokens pins the required max_tokens parameter when the request
// leaves MaxOutputTokens unset: the Messages API rejects requests without
// it, and inventing a per-model figure would be a silent behavior choice.
const DefaultMaxTokens = 8192

// ---------------------------------------------------------------------------
// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id         string
	reasonings []rawReasoning // thinking / redacted blocks in order
	texts      []string       // text blocks in order
	toolCalls  []rawToolCall
	finish     inference.FinishReason
	usage      rawUsage
}

// rawReasoning lowers one thinking block. Text is empty for redacted blocks,
// whose opaque data rides the signature slot.
type rawReasoning struct {
	text      string
	signature string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	inputTokens       int64
	outputTokens      int64
	cacheReadTokens   int64
	cacheWriteTokens  int64
	cacheWrite5m      int64
	cacheWrite1h      int64
	thinkingTokens    int64
	webSearchRequests int64
	webFetchRequests  int64
}

// streamRaw is one provider stream event. The streaming transport assigns
// canonical part indices (it is the stateful stage) so the decoder function
// stays pure and concurrency-safe.
type streamRaw struct {
	kind       streamRawKind
	part       int    // canonical part index (text / tool / reasoning kinds)
	text       string // text / thinking delta
	signature  string // terminal reasoning signature (redacted data included)
	responseID string // message id captured at message_start
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

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

// partField resolves a part kind to its ledger field through core's table, so
// the driver cannot drift from the field list the runtime activates. A miss
// cannot happen — core's TestGenerateLedgerCoversPartKinds pins the table
// against message.PartKinds — and panics rather than returning an empty field,
// because a decision recorded against "" never reaches the report.
func partField(
	lookup func(message.PartKind) (inference.FieldID, bool),
) func(message.PartKind) inference.FieldID {
	return func(kind message.PartKind) inference.FieldID {
		field, ok := lookup(kind)
		if !ok {
			panic("anthropic: no ledger field for part kind " + string(kind))
		}
		return field
	}
}

var (
	contextPartField = partField(inference.GenerateContextPartField)
	inputPartField   = partField(inference.GenerateInputPartField)
)

// compileGenerate lowers a canonical request into the provider wire. It never
// downgrades silently: parts the model cannot consume natively are rejected
// in the ledger with a precise reason.
func compileGenerate(
	modelName string,
	entry catalogEntry,
) inference.GenerateCompiler[anthropicgo.MessageNewParams] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[anthropicgo.MessageNewParams], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		params := anthropicgo.MessageNewParams{
			Model:     anthropicgo.Model(modelName),
			MaxTokens: DefaultMaxTokens,
		}
		if len(request.RequestMetadata) > 0 {
			ledger.Drop(
				inference.FieldGenerateRequestMetadata,
				"anthropic Messages API has no arbitrary request metadata channel",
			)
		}

		// Context messages. The system channel joins the request's system
		// blocks; everything else becomes user/assistant turns.
		for _, turn := range request.Context {
			switch turn.Role {
			case message.RoleSystem:
				compileSystem(&params, turn.Content.Parts, contextPartField, ledger)
			case message.RoleTool:
				compileToolResults(&params, turn.Content.Parts, entry, contextPartField, ledger)
			default: // user / assistant
				compileMessage(&params, turnRole(turn.Role), turn.Content.Parts, entry, contextPartField, ledger)
			}
		}

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(&params, request.Input.Content.Parts, entry, inputPartField, ledger)
		default:
			compileMessage(&params, anthropicgo.MessageParamRoleUser, request.Input.Content.Parts, entry, inputPartField, ledger)
		}

		compileIntent(&params, request.Input.Content.Intent, entry, ledger)

		// No provider extensions exist yet; anything attached is rejected
		// truthfully rather than dropped.
		ledger.RejectExtensions("generate", request.Extensions)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[anthropicgo.MessageNewParams]{Report: report}, ledger.Err()
		}
		return inference.Compiled[anthropicgo.MessageNewParams]{Wire: params, Report: report}, nil
	}
}

// turnRole maps a canonical turn role onto the Messages role. Only the
// assistant turn is distinct: system and tool turns take their own compile
// paths, so everything left is user-side.
func turnRole(role message.Role) anthropicgo.MessageParamRole {
	if role == message.RoleAssistant {
		return anthropicgo.MessageParamRoleAssistant
	}
	return anthropicgo.MessageParamRoleUser
}

// appendBlock appends one content block under role, merging into the previous
// turn when roles collide: the Messages API rejects consecutive same-role
// turns, and tool results always ride a user-role message. Thinking blocks
// hoist ahead of the rest of their turn, which the API requires.
func appendBlock(
	params *anthropicgo.MessageNewParams,
	role anthropicgo.MessageParamRole,
	block anthropicgo.ContentBlockParamUnion,
) {
	if last := len(params.Messages) - 1; last >= 0 && params.Messages[last].Role == role {
		message := &params.Messages[last]
		if isThinkingBlock(block) {
			message.Content = hoistThinking(message.Content, block)
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

// isThinkingBlock reports whether the block belongs at the head of an
// assistant turn: the Messages API requires thinking and redacted blocks to
// precede all other blocks in their message.
func isThinkingBlock(block anthropicgo.ContentBlockParamUnion) bool {
	return block.OfThinking != nil || block.OfRedactedThinking != nil
}

// hoistThinking inserts a thinking block after the message's existing
// thinking prefix, preserving the relative order of thinking blocks while
// keeping them ahead of text and tool blocks.
func hoistThinking(
	blocks []anthropicgo.ContentBlockParamUnion,
	block anthropicgo.ContentBlockParamUnion,
) []anthropicgo.ContentBlockParamUnion {
	at := 0
	for at < len(blocks) && isThinkingBlock(blocks[at]) {
		at++
	}
	out := make([]anthropicgo.ContentBlockParamUnion, 0, len(blocks)+1)
	out = append(out, blocks[:at]...)
	out = append(out, block)
	return append(out, blocks[at:]...)
}

// compileSystem lowers system and developer messages into the request's
// system blocks. Only text survives: the system channel carries no images
// or tool blocks.
func compileSystem(
	params *anthropicgo.MessageNewParams,
	parts []message.Part,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			params.System = append(params.System, anthropicgo.TextBlockParam{Text: value.Text})
		case message.DataPart:
			params.System = append(params.System, anthropicgo.TextBlockParam{
				Text: "\n" + string(value.Value) + "\n",
			})
		default:
			ledger.Reject(
				fields(part.Kind()),
				"system blocks carry text only",
			)
		}
	}
}

// compileMessage appends one user or assistant turn's parts to the request.
// Tool calls stay assistant-side blocks; tool results move to the user role.
func compileMessage(
	params *anthropicgo.MessageNewParams,
	role anthropicgo.MessageParamRole,
	parts []message.Part,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			appendBlock(params, role, textBlock(value.Text))
		case message.ImagePart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
				ledger.Reject(fields(message.PartImage), "model does not accept image input")
				continue
			}
			appendBlock(params, role, imageBlock(value.Source))
		case message.AudioPart:
			ledger.Reject(fields(message.PartAudio), "audio input is not supported by claude models")
		case message.VideoPart:
			switch {
			case !entry.videoInput:
				ledger.Reject(
					fields(message.PartVideo),
					"endpoint does not accept video blocks (set spec.wire.video_input)",
				)
			case !slices.Contains(entry.capabilities.Inputs, message.PartVideo):
				ledger.Reject(fields(message.PartVideo), "model does not accept video input")
			case value.Source.Kind() == media.SourceStream:
				ledger.Reject(
					fields(message.PartVideo),
					"stream media sources must be materialized before generate",
				)
			default:
				appendBlock(params, role, videoBlock(value.Source))
			}
		case message.FilePart:
			ledger.Reject(fields(message.PartFile), "file references are not supported")
		case message.DataPart:
			appendBlock(params, role, dataBlock(value.Value))
		case message.ToolCallPart:
			appendBlock(
				params,
				anthropicgo.MessageParamRoleAssistant,
				toolUseBlock(value.Call.ID, value.Call.Name, value.Call.Arguments),
			)
		case message.ToolResultPart:
			appendBlock(
				params,
				anthropicgo.MessageParamRoleUser,
				toolResultBlock(
					value.Result.CallID,
					compileToolResultContent(
						value.Result.Content,
						entry,
						fields(message.PartToolResult),
						ledger,
					),
				),
			)
		case message.ReasoningPart:
			compileReasoning(params, role, value, fields, ledger)
		}
	}
}

// compileReasoning lowers an assistant reasoning trace into thinking blocks.
// The Messages API verifies thinking blocks against their signature on
// round-trip, so unsigned reasoning cannot be forwarded honestly and is
// dropped with the reason on the ledger; redacted traces (empty text)
// round-trip through the opaque data slot.
func compileReasoning(
	params *anthropicgo.MessageNewParams,
	role anthropicgo.MessageParamRole,
	part message.ReasoningPart,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	field := fields(message.PartReasoning)
	if role != anthropicgo.MessageParamRoleAssistant {
		ledger.Reject(field, "reasoning parts belong to assistant context")
		return
	}
	if part.Signature == "" {
		ledger.Drop(field, "unsigned reasoning cannot round-trip through the messages api")
		return
	}
	if part.Text == "" {
		appendBlock(params, role, redactedThinkingBlock(part.Signature))
		return
	}
	appendBlock(params, role, thinkingBlock(part.Text, part.Signature))
}

// compileToolResults appends tool-role content as user-side tool_result
// blocks.
func compileToolResults(
	params *anthropicgo.MessageNewParams,
	parts []message.Part,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	for _, part := range parts {
		result, ok := part.(message.ToolResultPart)
		if !ok {
			ledger.Reject(
				fields(part.Kind()),
				"tool-role content carries tool results only",
			)
			continue
		}
		appendBlock(
			params,
			anthropicgo.MessageParamRoleUser,
			toolResultBlock(
				result.Result.CallID,
				compileToolResultContent(
					result.Result.Content,
					entry,
					fields(message.PartToolResult),
					ledger,
				),
			),
		)
	}
}

// compileToolResultContent lowers one tool result's content parts. Text and
// structured data always ride; an image needs a model that declares image
// input and a materialized source. The tool_result content union carries text
// and images only, so a video becomes a placeholder even where the endpoint
// accepts video blocks elsewhere. A part that cannot ride is replaced in place
// by a text placeholder, so the model sees where something was missing, and
// the loss lands on the ledger.
func compileToolResultContent(
	content message.Content,
	entry catalogEntry,
	field inference.FieldID,
	ledger *inference.Ledger,
) []anthropicgo.ToolResultBlockParamContentUnion {
	vision := slices.Contains(entry.capabilities.Inputs, message.PartImage)
	out := make([]anthropicgo.ToolResultBlockParamContentUnion, 0, len(content.Parts))
	omitted := make([]string, 0, len(content.Parts))
	for _, part := range content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			out = append(out, toolResultText(value.Text))
		case message.DataPart:
			out = append(out, toolResultText("\n"+string(value.Value)+"\n"))
		case message.ImagePart:
			reason := ""
			switch {
			case !vision:
				reason = "image (model does not accept image input)"
			case value.Source.Kind() == media.SourceStream:
				reason = "image (stream source was not materialized)"
			}
			if reason != "" {
				omitted = append(omitted, reason)
				out = append(out, toolResultText(toolResultPlaceholder(reason)))
				continue
			}
			out = append(out, anthropicgo.ToolResultBlockParamContentUnion{
				OfImage: &anthropicgo.ImageBlockParam{Source: imageSource(value.Source)},
			})
		case message.VideoPart:
			reason := "video (tool results carry text and images only)"
			omitted = append(omitted, reason)
			out = append(out, toolResultText(toolResultPlaceholder(reason)))
		default:
			reason := string(part.Kind())
			omitted = append(omitted, reason)
			out = append(out, toolResultText(toolResultPlaceholder(reason)))
		}
	}
	if len(omitted) > 0 {
		ledger.Drop(field, "tool output omitted "+strings.Join(omitted, ", "))
	}
	return out
}

func toolResultText(text string) anthropicgo.ToolResultBlockParamContentUnion {
	return anthropicgo.ToolResultBlockParamContentUnion{
		OfText: &anthropicgo.TextBlockParam{Text: text},
	}
}

// toolResultPlaceholder keeps a dropped part's slot in the result list.
func toolResultPlaceholder(reason string) string {
	return "[omitted tool output: " + reason + "]"
}

// The block constructors below are the only place the compiler speaks the
// SDK's content union: each owns one wire shape, and the compile loop reads as
// what the part means.

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

// toolParam lowers one canonical tool definition into the SDK's tool union.
func toolParam(definition message.ToolDefinition) anthropicgo.ToolUnionParam {
	tool := anthropicgo.ToolParam{
		Name:        definition.Name,
		InputSchema: toolInputSchema(definition.InputSchema),
	}
	if definition.Description != "" {
		tool.Description = param.NewOpt(definition.Description)
	}
	return anthropicgo.ToolUnionParam{OfTool: &tool}
}

// toolResultBlock keeps the string form for a single text part — every model
// accepts it verbatim — and rides the content list only when the result
// carries more.
func toolResultBlock(
	callID string,
	content []anthropicgo.ToolResultBlockParamContentUnion,
) anthropicgo.ContentBlockParamUnion {
	if len(content) <= 1 && (len(content) == 0 || content[0].OfText != nil) {
		text := ""
		if len(content) == 1 {
			text = content[0].OfText.Text
		}
		return anthropicgo.NewToolResultBlock(callID, text, false)
	}
	return anthropicgo.ContentBlockParamUnion{
		OfToolResult: &anthropicgo.ToolResultBlockParam{
			ToolUseID: callID,
			Content:   content,
			IsError:   anthropicgo.Bool(false),
		},
	}
}

// imageBlock and videoBlock lower media sources: URLs pass through, inline
// bytes carry their raw data plus media type for base64 encoding.
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

// imageSource builds the source union a tool_result image entry carries.
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

// videoBlock carries a compatible-endpoint extension the SDK does not model
// yet, so it rides a raw union; Anthropic's own schema has no video content.
func videoBlock(source media.VideoSource) anthropicgo.ContentBlockParamUnion {
	body := map[string]any{"type": "url", "url": source.URL()}
	if source.Kind() != media.SourceURL {
		body = map[string]any{
			"type":       "base64",
			"media_type": source.MediaType(),
			"data":       base64Encode(source.Bytes()),
		}
	}
	raw, _ := json.Marshal(map[string]any{"type": "video", "source": body})
	return param.Override[anthropicgo.ContentBlockParamUnion](json.RawMessage(raw))
}

func compileIntent(
	params *anthropicgo.MessageNewParams,
	intent inference.Intent,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	// thinking and effort are collected first: the Messages API carries one
	// reasoning control per request, and the dialect switch below resolves
	// which field carries it.
	var (
		thinking *bool
		effort   string
	)
	text := intent.Text
	if text != nil {
		if format := text.Response; format != nil {
			switch format.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject:
				ledger.Reject(
					inference.FieldGenerateIntentTextResponseKind,
					"claude has no json_object mode; supply a schema for structured output",
				)
			case inference.ResponseJSONSchema:
				params.OutputConfig.Format = anthropicgo.JSONOutputFormatParam{
					Schema: schemaMap(format.Schema),
				}
			}
		}
		if text.MaxOutputTokens != nil {
			params.MaxTokens = int64(*text.MaxOutputTokens)
		}
	}
	if intent.Image != nil {
		ledger.Reject(
			inference.FieldGenerateIntentImage,
			"claude models do not generate images",
		)
	}
	if intent.Audio != nil {
		ledger.Reject(
			inference.FieldGenerateIntentAudio,
			"claude models do not synthesize speech",
		)
	}
	if intent.Video != nil {
		ledger.Reject(
			inference.FieldGenerateIntentVideo,
			"claude models do not generate video",
		)
	}
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		params.Tools = append(params.Tools, toolParam(definition))
	}
	if choice := text.ToolChoice; choice != nil {
		params.ToolChoice = toolChoiceParam(*choice)
	}
	if text.Temperature != nil {
		params.Temperature = param.NewOpt(*text.Temperature)
	}
	if text.TopP != nil {
		params.TopP = param.NewOpt(*text.TopP)
	}
	if text.ReasoningEnabled != nil {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking to switch",
			)
		case entry.capabilities.Reasoning.Kind == model.ReasoningAlways &&
			!*text.ReasoningEnabled:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model cannot disable thinking",
			)
		default:
			enabled := *text.ReasoningEnabled
			thinking = &enabled
		}
	}
	if text.ReasoningEffort != "" {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no reasoning effort control",
			)
		case len(entry.capabilities.Reasoning.EffortMap) == 0:
			// The platform's thinking is binary: the level cannot be
			// honored, but the request for reasoning itself is — turn
			// thinking on and report the loss.
			on := true
			thinking = &on
			ledger.Drop(
				inference.FieldGenerateIntentReasoningEffort,
				"platform thinking has no effort levels; enabled at platform-chosen depth",
			)
		default:
			mode, _ := entry.capabilities.Reasoning.ResolveEffort(
				text.ReasoningEffort,
			)
			effort = mode
			if mode != string(text.ReasoningEffort) {
				ledger.Drop(
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
	// The Messages API expresses one reasoning control per request: an
	// explicit disable wins, then an effort level, then adaptive thinking.
	switch {
	case thinking != nil && !*thinking:
		params.Thinking = anthropicgo.ThinkingConfigParamUnion{
			OfDisabled: &anthropicgo.ThinkingConfigDisabledParam{},
		}
	case effort != "":
		params.OutputConfig.Effort = anthropicgo.OutputConfigEffort(effort)
	case thinking != nil && *thinking:
		params.Thinking = anthropicgo.ThinkingConfigParamUnion{
			OfAdaptive: &anthropicgo.ThinkingConfigAdaptiveParam{},
		}
	}
}

// schemaMap lowers a canonical JSON schema into the map shape the SDK's
// param types require; an empty schema becomes an open object schema.
func schemaMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{"type": "object"}
	}
	return decoded
}

// base64Encode renders inline image bytes for the transport.
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
