package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	openaishared "github.com/GizClaw/flowcraft/sdkx/llm/openai/shared"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	oairesponses "github.com/openai/openai-go/responses"
	oaishared "github.com/openai/openai-go/shared"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// LLM implements llm.LLM using OpenAI's Responses API.
type LLM struct {
	client   *oai.Client
	model    string
	provider string
}

var _ llm.LLM = (*LLM)(nil)

func New(model, apiKey, baseURL string, extraOpts ...option.RequestOption) (*LLM, error) {
	if model == "" {
		model = openaishared.DefaultModel
	}
	return &LLM{
		client:   openaishared.NewClient(apiKey, baseURL, extraOpts...),
		model:    model,
		provider: openaishared.DefaultProviderName,
	}, nil
}

// WithProviderName overrides the OTel / metrics provider tag used by
// this LLM instance. Wrapping adapters (sdkx/llm/azure, deepseek, qwen,
// ...) call this so each sub-provider's calls land under its own
// name in traces and metric labels instead of being aggregated under
// generic "openai". Returns the receiver for chaining; safe to ignore
// the return value. Empty names are silently ignored to keep the
// default intact when a caller passes an unset config.
func (c *LLM) WithProviderName(name string) *LLM {
	if c != nil && name != "" {
		c.provider = name
	}
	return c
}

func (c *LLM) Provider() string {
	if c == nil || c.provider == "" {
		return openaishared.DefaultProviderName
	}
	return c.provider
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.responses.generate.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	params, err := c.buildParams(messages, options)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}

	start := time.Now()
	resp, err := c.client.Responses.New(ctx, params, openaishared.ExtraRequestOpts(options)...)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.FromContext(fmt.Errorf("%s.responses.generate: %s: %w", provider, dur.String(), err))
		}
		return llm.Message{}, llm.TokenUsage{}, openaishared.ClassifyAPIErrorWithProvider(provider, err)
	}
	if resp == nil {
		err := errdefs.NotAvailablef("%s: nil responses response with no error (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}
	if err := validateResponse(provider, resp); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	msg := responseMessage(resp)
	usage := responseUsage(resp)
	span.SetAttributes(llm.UsageSpanAttrs(usage)...)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, provider, c.model, "success", dur, usage)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.responses.stream.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))

	options := llm.ApplyOptions(opts...)
	params, err := c.buildParams(messages, options)
	if err != nil {
		span.End()
		return nil, err
	}
	start := time.Now()
	stream := c.client.Responses.NewStreaming(ctx, params, openaishared.ExtraRequestOpts(options)...)
	if stream == nil {
		err := errdefs.NotAvailablef("%s: nil responses stream handle (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	if err := stream.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errdefs.FromContext(fmt.Errorf("%s.responses.stream: %s: %w: %w", provider, time.Since(start).String(), err, ctxErr))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return nil, err
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, openaishared.ClassifyAPIErrorWithProvider(provider, err)
	}
	return newStreamMessage(ctx, span, provider, c.model, stream), nil
}

func (c *LLM) buildParams(msgs []llm.Message, opts *llm.GenerateOptions) (oairesponses.ResponseNewParams, error) {
	input, instructions, err := convertInput(msgs)
	if err != nil {
		return oairesponses.ResponseNewParams{}, err
	}
	params := oairesponses.ResponseNewParams{
		Model: oaishared.ResponsesModel(c.model),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: input,
		},
	}
	if instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if opts.MaxTokens != nil {
		params.MaxOutputTokens = param.NewOpt(*opts.MaxTokens)
	}
	if opts.TopP != nil {
		params.TopP = param.NewOpt(*opts.TopP)
	}
	if opts.JSONSchema != nil {
		schema, err := schemaMap(opts.JSONSchema.Schema)
		if err != nil {
			return oairesponses.ResponseNewParams{}, err
		}
		format := oairesponses.ResponseFormatTextJSONSchemaConfigParam{
			Name:   opts.JSONSchema.Name,
			Schema: schema,
		}
		if opts.JSONSchema.Description != "" {
			format.Description = param.NewOpt(opts.JSONSchema.Description)
		}
		if opts.JSONSchema.Strict {
			format.Strict = param.NewOpt(true)
		}
		params.Text.Format = oairesponses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &format}
	} else if opts.JSONMode != nil && *opts.JSONMode {
		params.Text.Format = oairesponses.ResponseFormatTextConfigUnionParam{
			OfJSONObject: &oaishared.ResponseFormatJSONObjectParam{},
		}
	}
	if len(opts.Tools) > 0 {
		params.Tools = convertTools(opts.Tools)
	}
	if opts.ToolChoice != nil {
		params.ToolChoice = convertToolChoice(*opts.ToolChoice)
	}
	if key := openaishared.ComputePromptCacheKey(msgs, opts.Tools); key != "" {
		params.PromptCacheKey = param.NewOpt(key)
	}
	return params, nil
}

func convertInput(msgs []llm.Message) (oairesponses.ResponseInputParam, string, error) {
	var input oairesponses.ResponseInputParam
	var system []string
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleSystem:
			if err := openaishared.ValidateSystemTextParts(msg.Parts); err != nil {
				return nil, "", err
			}
			if text := strings.TrimSpace(msg.Content()); text != "" {
				system = append(system, text)
			}
		case llm.RoleTool:
			for _, result := range msg.ToolResults() {
				input = append(input, oairesponses.ResponseInputItemParamOfFunctionCallOutput(result.ToolCallID, result.Content))
			}
		case llm.RoleAssistant:
			text, err := openaishared.TextContent(msg.Parts)
			if err != nil {
				return nil, "", err
			}
			if strings.TrimSpace(text) != "" {
				input = append(input, easyMessage(oairesponses.EasyInputMessageRoleAssistant, text))
			}
			for _, call := range msg.ToolCalls() {
				input = append(input, oairesponses.ResponseInputItemParamOfFunctionCall(call.Arguments, call.ID, call.Name))
			}
		case llm.RoleUser:
			item, ok, err := userMessage(msg)
			if err != nil {
				return nil, "", err
			}
			if ok {
				input = append(input, item)
			}
		}
	}
	return input, strings.Join(system, "\n\n"), nil
}

func userMessage(msg llm.Message) (oairesponses.ResponseInputItemUnionParam, bool, error) {
	var content oairesponses.ResponseInputMessageContentListParam
	for _, p := range msg.Parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				content = append(content, oairesponses.ResponseInputContentParamOfInputText(p.Text))
			}
		case llm.PartData:
			if p.Data != nil {
				text, err := openaishared.FormatDataPartText(p.Data)
				if err != nil {
					return oairesponses.ResponseInputItemUnionParam{}, false, err
				}
				content = append(content, oairesponses.ResponseInputContentParamOfInputText(text))
			}
		case llm.PartImage:
			if p.Image != nil {
				if url := mediaURL(p.Image); url != "" {
					image := oairesponses.ResponseInputImageParam{Detail: oairesponses.ResponseInputImageDetailAuto}
					image.ImageURL = param.NewOpt(url)
					content = append(content, oairesponses.ResponseInputContentUnionParam{OfInputImage: &image})
				}
			}
		case llm.PartFile:
			if p.File != nil {
				content = append(content, fileContent(p.File))
			}
		case llm.PartAudio:
			if p.Audio != nil {
				return oairesponses.ResponseInputItemUnionParam{}, false, errdefs.Validationf("openai responses: role user does not support part type %s", p.Type)
			}
		case llm.PartToolCall:
			if p.ToolCall != nil {
				return oairesponses.ResponseInputItemUnionParam{}, false, errdefs.Validationf("openai responses: role user does not support part type %s", p.Type)
			}
		case llm.PartToolResult:
			if p.ToolResult != nil {
				return oairesponses.ResponseInputItemUnionParam{}, false, errdefs.Validationf("openai responses: role user does not support part type %s", p.Type)
			}
		default:
			if p.Type != "" {
				return oairesponses.ResponseInputItemUnionParam{}, false, errdefs.Validationf("openai responses: role user does not support part type %s", p.Type)
			}
		}
	}
	if len(content) == 0 {
		text := msg.Content()
		if text == "" {
			return oairesponses.ResponseInputItemUnionParam{}, false, nil
		}
		return easyMessage(oairesponses.EasyInputMessageRoleUser, text), true, nil
	}
	return easyContentMessage(oairesponses.EasyInputMessageRoleUser, content), true, nil
}

func easyMessage(role oairesponses.EasyInputMessageRole, text string) oairesponses.ResponseInputItemUnionParam {
	return oairesponses.ResponseInputItemParamOfMessage(text, role)
}

func easyContentMessage(role oairesponses.EasyInputMessageRole, content oairesponses.ResponseInputMessageContentListParam) oairesponses.ResponseInputItemUnionParam {
	return oairesponses.ResponseInputItemParamOfMessage(content, role)
}

func fileContent(f *llm.FileRef) oairesponses.ResponseInputContentUnionParam {
	fp := oairesponses.ResponseInputFileParam{}
	switch {
	case strings.HasPrefix(f.URI, "file-"):
		fp.FileID = param.NewOpt(f.URI)
	case strings.HasPrefix(f.URI, "file_id://"):
		fp.FileID = param.NewOpt(strings.TrimPrefix(f.URI, "file_id://"))
	case strings.HasPrefix(f.URI, "data:"):
		fp.FileData = param.NewOpt(f.URI)
	default:
		fp.FileURL = param.NewOpt(f.URI)
	}
	if f.Name != "" {
		fp.Filename = param.NewOpt(f.Name)
	}
	return oairesponses.ResponseInputContentUnionParam{OfInputFile: &fp}
}

func mediaURL(media *llm.MediaRef) string {
	if media.URL != "" {
		return media.URL
	}
	if media.Base64 == "" {
		return ""
	}
	mime := media.MediaType
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + media.Base64
}

func convertTools(tools []llm.ToolDefinition) []oairesponses.ToolUnionParam {
	out := make([]oairesponses.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		tool := oairesponses.ToolParamOfFunction(t.Name, t.InputSchema, false)
		if tool.OfFunction != nil && t.Description != "" {
			tool.OfFunction.Description = param.NewOpt(t.Description)
		}
		out = append(out, tool)
	}
	return out
}

func convertToolChoice(tc llm.ToolChoice) oairesponses.ResponseNewParamsToolChoiceUnion {
	switch tc.Type {
	case llm.ToolChoiceAuto:
		return oairesponses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(oairesponses.ToolChoiceOptionsAuto)}
	case llm.ToolChoiceNone:
		return oairesponses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(oairesponses.ToolChoiceOptionsNone)}
	case llm.ToolChoiceRequired:
		return oairesponses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(oairesponses.ToolChoiceOptionsRequired)}
	case llm.ToolChoiceSpecific:
		return oairesponses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &oairesponses.ToolChoiceFunctionParam{Name: tc.Name},
		}
	default:
		return oairesponses.ResponseNewParamsToolChoiceUnion{}
	}
}

func validateResponse(provider string, resp *oairesponses.Response) error {
	switch resp.Status {
	case "", oairesponses.ResponseStatusCompleted:
		if len(resp.Output) == 0 {
			return errdefs.NotAvailablef("%s responses: empty output", provider)
		}
		return nil
	case oairesponses.ResponseStatusFailed:
		return classifyResponseError(provider, "response failed", string(resp.Error.Code), resp.Error.Message)
	case oairesponses.ResponseStatusIncomplete:
		return classifyResponseIncomplete(provider, resp.IncompleteDetails.Reason)
	case oairesponses.ResponseStatusCancelled:
		return errdefs.Abortedf("%s responses: response cancelled", provider)
	case oairesponses.ResponseStatusQueued, oairesponses.ResponseStatusInProgress:
		return errdefs.NotAvailablef("%s responses: response %s", provider, resp.Status)
	default:
		return errdefs.NotAvailablef("%s responses: unexpected response status %q", provider, resp.Status)
	}
}

func classifyResponseError(provider, prefix, code, message string) error {
	msg := strings.TrimSpace(prefix)
	if code != "" {
		msg += " " + code
	}
	if message != "" {
		msg += ": " + message
	}
	err := errdefs.Fmt("%s responses: %s", provider, msg)
	switch lower := strings.ToLower(code + " " + message); {
	case strings.Contains(lower, "rate"):
		return errdefs.RateLimit(err)
	case strings.Contains(lower, "auth") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "permission denied"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "invalid") || strings.Contains(lower, "badrequest") || strings.Contains(lower, "notfound") || strings.Contains(lower, "context"):
		return errdefs.Validation(err)
	default:
		return errdefs.ClassifyProviderError(provider, err)
	}
}

func classifyResponseIncomplete(provider, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errdefs.NotAvailablef("%s responses: response incomplete", provider)
	}
	switch strings.ToLower(reason) {
	case "max_output_tokens", "content_filter":
		return errdefs.Validationf("%s responses: response incomplete: %s", provider, reason)
	default:
		return errdefs.NotAvailablef("%s responses: response incomplete: %s", provider, reason)
	}
}

func responseMessage(resp *oairesponses.Response) llm.Message {
	var parts []llm.Part
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Text != "" {
					parts = append(parts, llm.Part{Type: llm.PartText, Text: content.Text})
				}
				if content.Refusal != "" {
					parts = append(parts, llm.Part{Type: llm.PartText, Text: content.Refusal})
				}
			}
		case "function_call":
			parts = append(parts, llm.Part{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			}})
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func responseUsage(resp *oairesponses.Response) llm.TokenUsage {
	usage := resp.Usage
	out := llm.TokenUsage{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.TotalTokens,
		CachedInputTokens: usage.InputTokensDetails.CachedTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

func schemaMap(schema any) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	if m, ok := schema.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, errdefs.Validationf("openai responses: marshal json schema: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errdefs.Validationf("openai responses: json schema must be an object: %v", err)
	}
	return out, nil
}
