package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	asdk "github.com/anthropics/anthropic-sdk-go"
	sdkopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultModel     = "claude-sonnet-4-20250514"
	defaultMaxTokens = int64(4096)
)

func init() {
	llm.RegisterProvider("anthropic", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL, nil)
	})

	llm.RegisterProviderModels("anthropic", []llm.ModelInfo{
		{Label: "Claude Opus 4.6", Name: "claude-opus-4-6"},
		{Label: "Claude Sonnet 4.6", Name: "claude-sonnet-4-6"},
		{Label: "Claude Haiku 4.5", Name: "claude-4-5-haiku-20251001"},
	})
}

// LLM implements llm.LLM using the Anthropic SDK.
type LLM struct {
	client asdk.Client
	model  asdk.Model
}

var _ llm.LLM = (*LLM)(nil)

// New creates an Anthropic LLM instance.
func New(model, apiKey, baseURL string, httpClient *http.Client) (*LLM, error) {
	if model == "" {
		model = defaultModel
	}
	var ropts []sdkopt.RequestOption
	if apiKey != "" {
		ropts = append(ropts, sdkopt.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		ropts = append(ropts, sdkopt.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		ropts = append(ropts, sdkopt.WithHTTPClient(httpClient))
	}
	client := asdk.NewClient(ropts...)
	return &LLM{client: client, model: asdk.Model(model)}, nil
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.anthropic.generate.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "anthropic"),
		attribute.String("llm.model", string(c.model)),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	maxTokens := defaultMaxTokens
	if options.MaxTokens != nil {
		maxTokens = *options.MaxTokens
	}

	sys, msgParams, err := convertMessages(messages)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("anthropic: %w", err)
	}

	// JSON mode uses the Beta API with structured output.
	if options.JSONMode != nil && *options.JSONMode {
		p := asdk.BetaMessageNewParams{
			MaxTokens: maxTokens,
			Model:     c.model,
			Messages:  convertToBetaMessageParams(msgParams),
			System:    convertToBetaSystemBlocks(sys),
			OutputFormat: asdk.BetaJSONOutputFormatParam{
				Schema: map[string]any{
					"type":                 "object",
					"additionalProperties": true,
				},
			},
		}
		applyBetaOptions(&p, options)

		start := time.Now()
		resp, err := c.client.Beta.Messages.New(ctx, p)
		dur := time.Since(start)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			llm.RecordLLMMetrics(ctx, "anthropic", string(c.model), "error", dur, llm.TokenUsage{})
			if ctx.Err() != nil {
				return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("anthropic.generate: %s", err.Error())
			}
			return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("anthropic: %w", err)
		}

		text := extractBetaText(resp.Content)
		usage := llm.TokenUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
		span.SetAttributes(
			attribute.Int64("llm.input_tokens", usage.InputTokens),
			attribute.Int64("llm.output_tokens", usage.OutputTokens),
		)
		span.SetStatus(codes.Ok, "OK")
		llm.RecordLLMMetrics(ctx, "anthropic", string(c.model), "success", dur, usage)
		return llm.NewTextMessage(llm.RoleAssistant, text), usage, nil
	}

	p := asdk.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     c.model,
		Messages:  msgParams,
		System:    sys,
	}
	applyOptions(&p, options)

	start := time.Now()
	resp, err := c.client.Messages.New(ctx, p)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "anthropic", string(c.model), "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("anthropic.generate: %s", err.Error())
		}
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("anthropic: %w", err)
	}

	msg := convertResponse(resp.Content)
	usage := llm.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", usage.InputTokens),
		attribute.Int64("llm.output_tokens", usage.OutputTokens),
	)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "anthropic", string(c.model), "success", dur, usage)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.anthropic.stream.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "anthropic"),
		attribute.String("llm.model", string(c.model)),
	))

	options := llm.ApplyOptions(opts...)
	maxTokens := defaultMaxTokens
	if options.MaxTokens != nil {
		maxTokens = *options.MaxTokens
	}

	sys, msgParams, err := convertMessages(messages)
	if err != nil {
		span.End()
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	// JSON mode uses the Beta streaming API.
	if options.JSONMode != nil && *options.JSONMode {
		p := asdk.BetaMessageNewParams{
			MaxTokens: maxTokens,
			Model:     c.model,
			Messages:  convertToBetaMessageParams(msgParams),
			System:    convertToBetaSystemBlocks(sys),
			OutputFormat: asdk.BetaJSONOutputFormatParam{
				Schema: map[string]any{
					"type":                 "object",
					"additionalProperties": true,
				},
			},
		}
		applyBetaOptions(&p, options)

		stream := c.client.Beta.Messages.NewStreaming(ctx, p)
		return newBetaStreamMessage(ctx, span, string(c.model), stream), nil
	}

	p := asdk.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     c.model,
		Messages:  msgParams,
		System:    sys,
	}
	applyOptions(&p, options)

	stream := c.client.Messages.NewStreaming(ctx, p)
	return newStreamMessage(ctx, span, string(c.model), stream), nil
}

// --- message conversion ---

func convertMessages(messages []llm.Message) (system []asdk.TextBlockParam, out []asdk.MessageParam, err error) {
	var sysParts []string
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleSystem:
			if t := strings.TrimSpace(msg.Content()); t != "" {
				sysParts = append(sysParts, t)
			}
		case llm.RoleUser, llm.RoleAssistant:
			blocks, convErr := convertContentParts(msg.Parts)
			if convErr != nil {
				return nil, nil, convErr
			}
			if len(blocks) == 0 {
				continue
			}
			role := asdk.MessageParamRoleUser
			if msg.Role == llm.RoleAssistant {
				role = asdk.MessageParamRoleAssistant
			}
			mergeOrAppend(&out, role, blocks)
		case llm.RoleTool:
			var toolResults []asdk.ContentBlockParamUnion
			for _, r := range msg.ToolResults() {
				toolResults = append(toolResults, asdk.NewToolResultBlock(r.ToolCallID, r.Content, r.IsError))
			}
			if len(toolResults) > 0 {
				mergeOrAppend(&out, asdk.MessageParamRoleUser, toolResults)
			}
		default:
			return nil, nil, fmt.Errorf("anthropic: unsupported role %q", msg.Role)
		}
	}

	for i := range out {
		if len(out[i].Content) == 0 {
			out[i].Content = []asdk.ContentBlockParamUnion{asdk.NewTextBlock("")}
		}
	}

	if joined := strings.Join(sysParts, "\n"); strings.TrimSpace(joined) != "" {
		system = []asdk.TextBlockParam{{Text: joined}}
	}
	return system, out, nil
}

func mergeOrAppend(out *[]asdk.MessageParam, role asdk.MessageParamRole, blocks []asdk.ContentBlockParamUnion) {
	if n := len(*out); n > 0 && (*out)[n-1].Role == role {
		telemetry.Warn(context.Background(), "anthropic: merging consecutive messages with same role")
		(*out)[n-1].Content = append((*out)[n-1].Content, blocks...)
		return
	}
	if role == asdk.MessageParamRoleUser {
		*out = append(*out, asdk.NewUserMessage(blocks...))
	} else {
		*out = append(*out, asdk.NewAssistantMessage(blocks...))
	}
}

func convertContentParts(parts []llm.Part) ([]asdk.ContentBlockParamUnion, error) {
	var out []asdk.ContentBlockParamUnion
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			out = append(out, asdk.NewTextBlock(p.Text))
		case llm.PartToolCall:
			if p.ToolCall != nil {
				out = append(out, asdk.NewToolUseBlock(p.ToolCall.ID, json.RawMessage(p.ToolCall.Arguments), p.ToolCall.Name))
			}
		case llm.PartImage:
			if p.Image != nil && strings.HasPrefix(p.Image.URL, "data:") {
				mediaType, b64, err := parseDataURL(p.Image.URL)
				if err != nil {
					return nil, err
				}
				out = append(out, asdk.NewImageBlockBase64(mediaType, b64))
			}
		case llm.PartFile:
			if p.File != nil {
				blk, err := convertFilePartAnthropic(p.File)
				if err != nil {
					return nil, err
				}
				if blk != nil {
					out = append(out, *blk)
				}
			}
		case llm.PartData:
			if p.Data != nil {
				b, _ := json.Marshal(p.Data.Value)
				out = append(out, asdk.NewTextBlock(string(b)))
			}
		}
	}
	return out, nil
}

func convertFilePartAnthropic(f *llm.FileRef) (*asdk.ContentBlockParamUnion, error) {
	mime := f.MimeType
	if strings.HasPrefix(mime, "image/") && strings.HasPrefix(f.URI, "data:") {
		mt, b64, err := parseDataURL(f.URI)
		if err != nil {
			return nil, err
		}
		blk := asdk.NewImageBlockBase64(mt, b64)
		return &blk, nil
	}
	if mime == "application/pdf" {
		if strings.HasPrefix(f.URI, "http://") || strings.HasPrefix(f.URI, "https://") {
			blk := asdk.NewDocumentBlock(asdk.URLPDFSourceParam{URL: f.URI})
			return &blk, nil
		}
		blk := asdk.NewDocumentBlock(asdk.PlainTextSourceParam{Data: f.URI})
		return &blk, nil
	}
	blk := asdk.NewDocumentBlock(asdk.PlainTextSourceParam{Data: f.URI})
	return &blk, nil
}

func convertResponse(blocks []asdk.ContentBlockUnion) llm.Message {
	var parts []llm.Part
	for _, blk := range blocks {
		switch v := blk.AsAny().(type) {
		case asdk.TextBlock:
			if v.Text != "" {
				parts = append(parts, llm.Part{Type: llm.PartText, Text: v.Text})
			}
		case asdk.ToolUseBlock:
			parts = append(parts, llm.Part{
				Type: llm.PartToolCall,
				ToolCall: &llm.ToolCall{
					ID:        v.ID,
					Name:      v.Name,
					Arguments: string(v.Input),
				},
			})
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

// --- Beta API helpers (JSON mode) ---

func applyBetaOptions(p *asdk.BetaMessageNewParams, options *llm.GenerateOptions) {
	if options.Temperature != nil {
		p.Temperature = param.NewOpt(*options.Temperature)
	}
	if options.TopP != nil {
		p.TopP = param.NewOpt(*options.TopP)
	}
	if options.TopK != nil {
		p.TopK = param.NewOpt(*options.TopK)
	}
	if len(options.StopWords) > 0 {
		p.StopSequences = options.StopWords
	}
}

func extractBetaText(blocks []asdk.BetaContentBlockUnion) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func convertToBetaMessageParams(msgs []asdk.MessageParam) []asdk.BetaMessageParam {
	out := make([]asdk.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		var blocks []asdk.BetaContentBlockParamUnion
		for _, b := range m.Content {
			if b.OfText != nil {
				blocks = append(blocks, asdk.BetaContentBlockParamUnion{OfText: &asdk.BetaTextBlockParam{Text: b.OfText.Text}})
			}
			if b.OfImage != nil {
				if src := b.OfImage.Source.OfBase64; src != nil {
					blocks = append(blocks, asdk.BetaContentBlockParamUnion{OfImage: &asdk.BetaImageBlockParam{
						Source: asdk.BetaImageBlockParamSourceUnion{
							OfBase64: &asdk.BetaBase64ImageSourceParam{
								Data:      src.Data,
								MediaType: asdk.BetaBase64ImageSourceMediaType(src.MediaType),
							},
						},
					}})
				}
			}
		}

		role := asdk.BetaMessageParamRoleUser
		if m.Role == asdk.MessageParamRoleAssistant {
			role = asdk.BetaMessageParamRoleAssistant
		}
		out = append(out, asdk.BetaMessageParam{Role: role, Content: blocks})
	}
	return out
}

func convertToBetaSystemBlocks(sys []asdk.TextBlockParam) []asdk.BetaTextBlockParam {
	if len(sys) == 0 {
		return nil
	}
	blocks := make([]asdk.BetaTextBlockParam, 0, len(sys))
	for _, s := range sys {
		blocks = append(blocks, asdk.BetaTextBlockParam{Text: s.Text})
	}
	return blocks
}

// --- Stable API helpers ---

func applyOptions(p *asdk.MessageNewParams, options *llm.GenerateOptions) {
	if options.Temperature != nil {
		p.Temperature = param.NewOpt(*options.Temperature)
	}
	if options.TopP != nil {
		p.TopP = param.NewOpt(*options.TopP)
	}
	if options.TopK != nil {
		p.TopK = param.NewOpt(*options.TopK)
	}
	if len(options.StopWords) > 0 {
		p.StopSequences = options.StopWords
	}

	if len(options.Tools) > 0 {
		tools := make([]asdk.ToolUnionParam, 0, len(options.Tools))
		for _, td := range options.Tools {
			schema := asdk.ToolInputSchemaParam{}
			if props, ok := td.InputSchema["properties"]; ok {
				schema.Properties = props
			}
			if req, ok := td.InputSchema["required"]; ok {
				switch v := req.(type) {
				case []string:
					schema.Required = v
				case []any:
					strs := make([]string, 0, len(v))
					for _, item := range v {
						if s, ok := item.(string); ok {
							strs = append(strs, s)
						}
					}
					schema.Required = strs
				}
			}
			tools = append(tools, asdk.ToolUnionParam{OfTool: &asdk.ToolParam{
				Name:        td.Name,
				Description: asdk.String(td.Description),
				InputSchema: schema,
			}})
		}
		p.Tools = tools
	}

	if options.ToolChoice != nil {
		switch options.ToolChoice.Type {
		case llm.ToolChoiceAuto:
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfAuto: &asdk.ToolChoiceAutoParam{}}
		case llm.ToolChoiceNone:
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfNone: &asdk.ToolChoiceNoneParam{}}
		case llm.ToolChoiceRequired:
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfAny: &asdk.ToolChoiceAnyParam{}}
		case llm.ToolChoiceSpecific:
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfTool: &asdk.ToolChoiceToolParam{Name: options.ToolChoice.Name}}
		}
	}
}

func parseDataURL(s string) (mediaType, base64Data string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "data:") {
		return "", "", fmt.Errorf("expected data url")
	}
	rest := strings.TrimPrefix(s, "data:")
	i := strings.Index(rest, ";base64,")
	if i <= 0 {
		return "", "", fmt.Errorf("invalid data url")
	}
	return rest[:i], rest[i+len(";base64,"):], nil
}
