package bytedance

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

// Raw model — transport-owned response data, decoded into canonical forms.
// ---------------------------------------------------------------------------

type generateRaw struct {
	id             string
	reasonings     []rawReasoning // reasoning items in output order
	texts          []string       // output_text items in order
	toolCalls      []rawToolCall
	webSearchCalls []inference.WebSearchCall
	citations      []inference.Citation
	finish         inference.FinishReason
	usage          rawUsage
	failedReason   string // non-empty when the provider reported failure
}

// rawReasoning lowers one reasoning item: joined summary text and the item
// id. ark signs nothing, so the trace cannot round-trip; it is display-only.
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
	totalTokens       int64
	cachedTokens      int64
	reasoningTokens   int64
	inputAudioTokens  int64
	webSearchRequests int64
	mcpRequests       int64
}

// streamRaw is one provider stream event. The streaming transport assigns
// canonical part indices (it is the stateful stage) so the decoder function
// stays pure and concurrency-safe.
type streamRaw struct {
	kind            streamRawKind
	part            int    // canonical part index (text / tool / reasoning kinds)
	text            string // text / summary delta
	id              string // terminal reasoning item id
	responseID      string // response id from the terminal event
	tool            streamRawTool
	usage           *rawUsage
	finish          inference.FinishReason
	providerOutputs inference.ProviderOutputs
}

type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawToolFragment
	streamRawReasoning
	streamRawProviderOutput
	streamRawFinish
)

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

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
			panic("bytedance: no ledger field for part kind " + string(kind))
		}
		return field
	}
}

var (
	contextPartField = partField(inference.GenerateContextPartField)
	inputPartField   = partField(inference.GenerateInputPartField)
	embedPartField   = partField(inference.EmbedItemPartField)
)

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

// compileGenerate lowers a canonical request into the provider wire. It never
// downgrades silently: parts the model cannot consume natively are rejected
// in the ledger with a precise reason.
// rejectTextControls rejects the text-only intent controls (tools, sampling,
// reasoning) for a non-text operation, one decision per active field so the
// report stays field-precise.
func rejectTextControls(
	text *inference.TextIntent,
	ledger *inference.Ledger,
	toolsReason, samplingReason, reasoningReason string,
) {
	if len(text.Tools) > 0 {
		ledger.Reject(inference.FieldGenerateIntentTools, toolsReason)
	}
	if text.ToolChoice != nil {
		ledger.Reject(inference.FieldGenerateIntentToolChoice, toolsReason)
	}
	if text.Temperature != nil {
		ledger.Reject(inference.FieldGenerateIntentTemperature, samplingReason)
	}
	if text.TopP != nil {
		ledger.Reject(inference.FieldGenerateIntentTopP, samplingReason)
	}
	if text.ReasoningEnabled != nil {
		ledger.Reject(inference.FieldGenerateIntentReasoningEnabled, reasoningReason)
	}
	if text.ReasoningEffort != "" {
		ledger.Reject(inference.FieldGenerateIntentReasoningEffort, reasoningReason)
	}
}

// sourceURI renders an image source as the single URI string the API accepts:
// absolute URLs pass through, inline bytes become a data: URI.
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

func bytesClone(raw []byte) []byte {
	return append([]byte(nil), raw...)
}

// compileGenerate lowers a canonical request into the Ark Responses request
// body. It never downgrades silently: parts the model cannot consume natively
// are rejected in the ledger with a precise reason.
func compileGenerate(
	endpoint string,
	entry catalogEntry,
) inference.GenerateCompiler[*arkresponses.ResponsesRequest] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[*arkresponses.ResponsesRequest], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		ark := &arkresponses.ResponsesRequest{Model: endpoint}
		if shape == inference.GenerateExecutionStream {
			stream := true
			ark.Stream = &stream
		}
		if len(request.RequestMetadata) > 0 {
			ledger.Drop(
				inference.FieldGenerateRequestMetadata,
				"bytedance Ark SDK has no arbitrary request metadata channel",
			)
		}

		// Context messages → items. System text folds into the native
		// instructions field; non-text system parts have no native home.
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
						ledger.Reject(
							contextPartField(part.Kind()),
							"system messages carry text only on the Responses API",
						)
					}
				}
			case message.RoleTool:
				compileToolResults(ark, turn.Content.Parts, contextPartField, ledger)
			default: // user / assistant
				compileMessage(ark, string(turn.Role), turn.Content.Parts, entry, contextPartField, ledger)
			}
		}
		if instructions := strings.Join(system, "\n\n"); instructions != "" {
			ark.Instructions = &instructions
		}

		// Current input.
		switch request.Input.Role {
		case inference.InputRoleTool:
			compileToolResults(ark, request.Input.Content.Parts, inputPartField, ledger)
		default:
			compileMessage(ark, "user", request.Input.Content.Parts, entry, inputPartField, ledger)
		}

		compileIntent(ark, request.Input.Content.Intent, entry, ledger)

		// Provider options: GenerateOptions fields lower onto the request one
		// by one; extensions for other operations are rejected wholesale.
		options, other := inference.ExtensionFor[GenerateOptions](request.Extensions)
		ledger.RejectExtensions("generate", other)
		compileGenerateOptions(ark, options, entry, ledger)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[*arkresponses.ResponsesRequest]{Report: report}, ledger.Err()
		}
		return inference.Compiled[*arkresponses.ResponsesRequest]{Wire: ark, Report: report}, nil
	}
}

// compileGenerateOptions lowers GenerateOptions onto the request.
func compileGenerateOptions(
	ark *arkresponses.ResponsesRequest,
	options GenerateOptions,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
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
		if !entry.capabilities.HostedWebSearch {
			ledger.Reject(
				inference.ExtensionField("web_search").Qualify(options),
				"model does not support hosted web search",
			)
			return
		}
		ark.Tools = append(ark.Tools, arkWebSearchTool(options.WebSearch))
	}
}

// compileMessage appends one user/assistant turn. The ark item model
// separates function calls from messages, so a message with interleaved text
// and tool parts becomes a run of message items plus call items in order.
func compileMessage(
	ark *arkresponses.ResponsesRequest,
	role string,
	parts []message.Part,
	entry catalogEntry,
	fields func(message.PartKind) inference.FieldID,
	ledger *inference.Ledger,
) {
	var content []*arkresponses.ContentItem
	flush := func() {
		if len(content) == 0 {
			return
		}
		appendInputItem(ark, arkMessageItem(role, content))
		content = nil
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			content = append(content, arkContentText(value.Text))
		case message.ImagePart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
				ledger.Reject(fields(message.PartImage), "model does not accept image input")
				continue
			}
			content = append(content, arkContentImage(sourceURI(value.Source)))
		case message.VideoPart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartVideo) {
				ledger.Reject(fields(message.PartVideo), "model does not accept video input")
				continue
			}
			content = append(content, arkContentVideo(videoSourceURI(value.Source)))
		case message.AudioPart:
			if !slices.Contains(entry.capabilities.Inputs, message.PartAudio) {
				ledger.Reject(fields(message.PartAudio), "model does not accept audio input")
				continue
			}
			content = append(content, arkContentAudio(audioSourceURI(value.Source)))
		case message.FilePart:
			ledger.Reject(fields(message.PartFile), "file references are not supported")
		case message.DataPart:
			content = append(content, arkContentText("\n"+string(value.Value)+"\n"))
		case message.ToolCallPart:
			flush()
			appendInputItem(ark, arkToolCallItem(
				value.Call.ID, value.Call.Name, value.Call.Arguments,
			))
		case message.ToolResultPart:
			flush()
			appendInputItem(ark, arkToolResultItem(
				value.Result.CallID,
				compileToolResultContent(
					value.Result.Content,
					fields(message.PartToolResult),
					ledger,
				),
			))
		case message.ReasoningPart:
			flush()
			field := fields(message.PartReasoning)
			if role != "assistant" {
				ledger.Reject(field, "reasoning parts belong to assistant context")
				continue
			}
			// ark emits reasoning traces but signs nothing and consumes no
			// reasoning input: the trace cannot round-trip, so it drops with
			// the reason on the ledger rather than vanishing.
			ledger.Drop(field, "ark does not consume reasoning input")
		}
	}
	flush()
}

// compileToolResults appends tool-role content. ark carries no error flag on
// tool outputs; the result content is preserved verbatim.
func compileToolResults(
	ark *arkresponses.ResponsesRequest,
	parts []message.Part,
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
		appendInputItem(ark, arkToolResultItem(
			result.Result.CallID,
			compileToolResultContent(
				result.Result.Content,
				fields(message.PartToolResult),
				ledger,
			),
		))
	}
}

// compileToolResultContent lowers a tool result's content into the string ark
// carries on function_call_output. That field has no content-list form, so
// text and structured data ride and everything else becomes an in-place text
// placeholder reported on the ledger: the model still sees where something was
// omitted, and the loss is auditable instead of silent.
func compileToolResultContent(
	content message.Content,
	field inference.FieldID,
	ledger *inference.Ledger,
) string {
	var builder strings.Builder
	omitted := make([]string, 0, len(content.Parts))
	for _, part := range content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			builder.WriteString(value.Text)
		case message.DataPart:
			builder.WriteString("\n")
			builder.Write(value.Value)
			builder.WriteString("\n")
		default:
			reason := string(part.Kind()) + " (ark tool output is text only)"
			omitted = append(omitted, reason)
			builder.WriteString("[omitted tool output: " + reason + "]")
		}
	}
	if len(omitted) > 0 {
		ledger.Drop(field, "tool output omitted "+strings.Join(omitted, ", "))
	}
	return builder.String()
}

// compileIntent lowers the output controls onto the request.
func compileIntent(
	ark *arkresponses.ResponsesRequest,
	intent inference.Intent,
	entry catalogEntry,
	ledger *inference.Ledger,
) {
	if text := intent.Text; text != nil {
		if format := text.Response; format != nil {
			switch format.Kind {
			case "", inference.ResponseText:
			case inference.ResponseJSONObject:
				ark.Text = &arkresponses.ResponsesText{
					Format: arkTextFormat("json_object", "", nil, false),
				}
			case inference.ResponseJSONSchema:
				ark.Text = &arkresponses.ResponsesText{
					Format: arkTextFormat(
						"json_schema", format.Name, format.Schema, true,
					),
				}
			}
		}
		if text.MaxOutputTokens != nil {
			max := int64(*text.MaxOutputTokens)
			ark.MaxOutputTokens = &max
		}
	}
	if intent.Image != nil {
		ledger.Reject(
			inference.FieldGenerateIntentImage,
			"text models do not generate images; route a seedream model",
		)
	}
	if intent.Audio != nil {
		ledger.Reject(
			inference.FieldGenerateIntentAudio,
			"text models do not synthesize speech",
		)
	}
	text := intent.Text
	if text == nil {
		return
	}
	for _, definition := range text.Tools {
		ark.Tools = append(ark.Tools, arkTool(definition))
	}
	if choice := text.ToolChoice; choice != nil {
		ark.ToolChoice = arkToolChoice(*choice)
	}
	ark.Temperature = text.Temperature
	ark.TopP = text.TopP
	if text.ReasoningEnabled != nil {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model has no thinking control",
			)
		case entry.capabilities.Reasoning.Kind == model.ReasoningAlways &&
			!*text.ReasoningEnabled:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEnabled,
				"model cannot disable thinking",
			)
		default:
			setArkThinking(ark, text.ReasoningEnabled, "")
		}
	}
	if text.ReasoningEffort != "" {
		switch {
		case entry.capabilities.Reasoning.Kind == model.ReasoningNone:
			ledger.Reject(
				inference.FieldGenerateIntentReasoningEffort,
				"model has no thinking control",
			)
		case len(entry.capabilities.Reasoning.EffortMap) == 0:
			// Spec-declared reasoning models without a dial: honor the
			// request for reasoning itself and report the lost level.
			on := true
			setArkThinking(ark, &on, "")
			ledger.Drop(
				inference.FieldGenerateIntentReasoningEffort,
				"model's thinking is binary; no effort dial exists",
			)
		default:
			mode, _ := entry.capabilities.Reasoning.ResolveEffort(
				text.ReasoningEffort,
			)
			setArkThinking(ark, text.ReasoningEnabled, mode)
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
}
