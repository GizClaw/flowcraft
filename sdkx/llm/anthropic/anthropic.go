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
	defaultModel                = "claude-opus-4-6"
	defaultMaxTokens            = int64(4096)
	defaultThinkingBudgetTokens = int64(1024)
)

// normalizeAnthropicUsage maps Anthropic's three-bucket prompt-token
// breakdown onto the sdk's two-field TokenUsage contract.
//
// Anthropic returns prompt tokens in three independent counters:
//
//   - inputTokens          – NEW, non-cached input on this call
//   - cacheReadInputTokens – cached subset served at ~10% read rate
//   - cacheCreationInputTokens – tokens written to cache this call
//     (1.25x rate on the SKUs that support it)
//
// `inputTokens` alone therefore UNDER-counts the wire prompt size,
// which is the cause of the historical "Anthropic TokenUsage looks
// smaller than the equivalent OpenAI call" complaint. The sdk
// contract for TokenUsage.InputTokens is the *gross* prompt size
// (matching OpenAI's `prompt_tokens` semantics), so this helper sums
// all three buckets. CachedInputTokens then names the cache-read
// subset alone — a uniform observable across providers for callers
// computing hit-rate via CachedInputTokens / InputTokens.
//
// Extracted as a named function so the three-bucket arithmetic
// (used identically in Generate, GenerateStream's beta and stable
// paths, plus their `updateUsage` event handlers) has a single
// regression-test surface.
func normalizeAnthropicUsage(inputTokens, cacheReadInputTokens, cacheCreationInputTokens int64) (gross, cached int64) {
	gross = inputTokens + cacheReadInputTokens + cacheCreationInputTokens
	cached = cacheReadInputTokens
	return gross, cached
}

func init() {
	llm.RegisterProvider("anthropic", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL, nil)
	})

	// Catalog reflects Anthropic's public model lineup as of 2026-04-30.
	// Sources:
	//   - https://www.anthropic.com/news/claude-opus-4-6
	//   - https://www.anthropic.com/news/claude-sonnet-4-6
	//   - https://platform.claude.com/docs/en/build-with-claude/context-windows
	//   - https://www-cdn.anthropic.com/...claude-opus-4-6-system-card.pdf
	//
	// CapJSONSchema is disabled for the entire family: the Anthropic
	// Messages API has no first-class schema-constrained structured
	// output mode (no equivalent to OpenAI's
	// `response_format: {"type": "json_schema", schema: …}`). This
	// adapter does NOT translate GenerateOptions.JSONSchema into tool
	// definitions either — see Generate / GenerateStream below; the
	// JSONSchema field is silently unused.
	//
	// CapJSONMode IS supported. The adapter routes JSONMode=true to
	// the Beta Messages API with BetaJSONOutputFormatParam (a generic
	// object schema), giving callers an "emit valid JSON" toggle that
	// is the moral equivalent of OpenAI's `json_object` mode.
	//
	// The caps-middleware downgrade rule (CapJSONSchema disabled →
	// set JSONMode=true) means callers asking for schema-constrained
	// output will land on JSONMode=true here without code changes.
	// Per platform.claude.com/docs/en/about-claude/models/overview
	// (read 2026-04-30) every current Claude SKU supports vision and
	// tool use, so only CapJSONSchema is disabled per family.
	//
	// Output modality: text only. Claude has no native image or
	// audio output across the 4.x family — vision is input-only per
	// docs.anthropic.com/en/docs/vision (analyse / understand,
	// not generate). Disable both output modality caps so policy
	// matching does not route image-output slots here.
	textOnlyOutput := llm.DisabledCaps(
		llm.CapJSONSchema,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	llm.RegisterProviderModels("anthropic", []llm.ModelInfo{
		{
			// New flagship as of 2026-04. 1M ctx / 128K output.
			// Source: https://www.anthropic.com/news/claude-opus-4-7
			Label: "Claude Opus 4.7",
			Name:  "claude-opus-4-7",
			Spec: llm.ModelSpec{
				Caps: textOnlyOutput,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			// Previous flagship; kept for callers pinned to it.
			// 1M ctx / 128K output per anthropic.com/news/claude-opus-4-6.
			Label: "Claude Opus 4.6",
			Name:  "claude-opus-4-6",
			Spec: llm.ModelSpec{
				Caps: textOnlyOutput,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			// Balanced workhorse. 1M ctx (beta header) / 64K output
			// per platform.claude.com models overview.
			Label: "Claude Sonnet 4.6",
			Name:  "claude-sonnet-4-6",
			Spec: llm.ModelSpec{
				Caps: textOnlyOutput,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  64_000,
				},
			},
		},
		{
			// Compact / fastest tier. 200K ctx / 64K output per
			// platform.claude.com models overview. Note: the API ID
			// follows the new family-first convention introduced
			// with the 4.x series — claude-haiku-4-5-{date}, not
			// claude-4-5-haiku-{date}.
			Label: "Claude Haiku 4.5",
			Name:  "claude-haiku-4-5-20251001",
			Spec: llm.ModelSpec{
				Caps: textOnlyOutput,
				Limits: llm.ModelLimits{
					MaxContextTokens: 200_000,
					MaxOutputTokens:  64_000,
				},
			},
		},
	})
}

// LLM implements llm.LLM using the Anthropic SDK.
type LLM struct {
	client asdk.Client
	model  asdk.Model

	// provider is the tag that lands on OTel spans, metrics, and the
	// fallback path of [classifyAPIError]'s error wrapping. It defaults
	// to "anthropic" so direct callers see the historical behaviour;
	// the sibling adapter sdkx/llm/minimax (which wraps this package
	// over the Anthropic-compatible /anthropic endpoint) calls
	// [LLM.WithProviderName] to override it so MiniMax traffic shows
	// up under "minimax" in observability tooling instead of being
	// silently aggregated under "anthropic". Same pattern as
	// sdkx/llm/openai for the OpenAI-compatible sub-providers.
	provider string
}

var _ llm.LLM = (*LLM)(nil)

// defaultProviderName is the OTel/metrics tag stamped on every direct
// anthropic.New call. Wrapping adapters override it via WithProviderName.
const defaultProviderName = "anthropic"

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
	return &LLM{client: client, model: asdk.Model(model), provider: defaultProviderName}, nil
}

// WithProviderName overrides the OTel / metrics provider tag used by
// this LLM instance. Wrapping adapters (sdkx/llm/minimax) call this
// so each sub-provider's calls land under its own name in traces and
// metric labels instead of being aggregated under generic "anthropic".
// Returns the receiver for chaining; safe to ignore the return value.
// Empty names are silently ignored to keep the default intact when a
// caller passes an unset config.
func (c *LLM) WithProviderName(name string) *LLM {
	if c != nil && name != "" {
		c.provider = name
	}
	return c
}

// Provider returns the OTel / metrics tag used by this instance. Mostly
// a debugging aid; exported so eval drivers and observability dashboards
// can introspect what name they'll see in traces.
func (c *LLM) Provider() string {
	if c == nil || c.provider == "" {
		return defaultProviderName
	}
	return c.provider
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.generate.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, string(c.model)),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	maxTokens := defaultMaxTokens
	if options.MaxTokens != nil {
		maxTokens = *options.MaxTokens
	}

	sys, msgParams, err := convertMessages(messages)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(fmt.Errorf("anthropic: %w", err))
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
		if err := applyBetaOptions(&p, options); err != nil {
			return llm.Message{}, llm.TokenUsage{}, err
		}
		// Plan anchors against the stable params (planCacheAnchors
		// reads only Text / content-length fields, which are
		// identical across the stable / beta type pair) and then
		// re-apply to the beta slices. JSON mode currently has no
		// tools surface, so the toolsLast anchor is dropped.
		plan := planCacheAnchors(sys, msgParams, nil)
		applyAnchorsToBetaSystem(p.System, plan.systemBlocks)
		applyAnchorToBetaHistory(p.Messages, plan.historyMsgIdx)

		start := time.Now()
		resp, err := c.client.Beta.Messages.New(ctx, p)
		dur := time.Since(start)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			llm.RecordLLMMetrics(ctx, provider, string(c.model), "error", dur, llm.TokenUsage{})
			if ctx.Err() != nil {
				return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("%s.generate: %s", provider, err.Error())
			}
			return llm.Message{}, llm.TokenUsage{}, c.classifyAPIError(err)
		}
		// anthropic-sdk-go and Anthropic-compatible backends (MiniMax via
		// /anthropic) have been observed returning (nil, nil) under flaky
		// network conditions; see the same guard in sdkx/llm/openai. The
		// "err==nil ⇒ resp!=nil" pointer-return convention is not a
		// language guarantee, and the alternative (raw deref) crashes
		// the whole runner.
		if resp == nil {
			err := errdefs.NotAvailablef("%s: nil beta response with no error (provider misbehaviour)", provider)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			llm.RecordLLMMetrics(ctx, provider, string(c.model), "error", dur, llm.TokenUsage{})
			return llm.Message{}, llm.TokenUsage{}, err
		}

		text := extractBetaText(resp.Content)
		gross, cached := normalizeAnthropicUsage(resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
		usage := llm.TokenUsage{
			InputTokens:       gross,
			CachedInputTokens: cached,
			OutputTokens:      resp.Usage.OutputTokens,
			TotalTokens:       gross + resp.Usage.OutputTokens,
		}
		span.SetAttributes(llm.UsageSpanAttrs(usage)...)
		span.SetStatus(codes.Ok, "OK")
		llm.RecordLLMMetrics(ctx, provider, string(c.model), "success", dur, usage)
		return llm.NewTextMessage(llm.RoleAssistant, text), usage, nil
	}

	p := asdk.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     c.model,
		Messages:  msgParams,
		System:    sys,
	}
	if err := applyOptions(&p, options); err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	// Cache-anchor planning must run *after* applyOptions has
	// populated p.Tools — the plan's tools-end anchor needs the
	// final tool slice to mutate in place. System / history
	// anchors operate on the same slices held by p.System and
	// p.Messages (Go pass-by-slice semantics), so mutating
	// through the local handles is observable on p.
	plan := planCacheAnchors(p.System, p.Messages, p.Tools)
	applyAnchorsToSystem(p.System, plan.systemBlocks)
	applyAnchorToHistory(p.Messages, plan.historyMsgIdx)
	if plan.toolsLast {
		applyAnchorToTools(p.Tools)
	}

	start := time.Now()
	resp, err := c.client.Messages.New(ctx, p)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, string(c.model), "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("%s.generate: %s", provider, err.Error())
		}
		return llm.Message{}, llm.TokenUsage{}, c.classifyAPIError(err)
	}
	// See nil-check rationale in the beta branch above and in
	// sdkx/llm/openai.
	if resp == nil {
		err := errdefs.NotAvailablef("%s: nil response with no error (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, string(c.model), "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	msg := convertResponse(resp.Content)
	gross, cached := normalizeAnthropicUsage(resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	usage := llm.TokenUsage{
		InputTokens:       gross,
		CachedInputTokens: cached,
		OutputTokens:      resp.Usage.OutputTokens,
		TotalTokens:       gross + resp.Usage.OutputTokens,
	}

	span.SetAttributes(llm.UsageSpanAttrs(usage)...)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, provider, string(c.model), "success", dur, usage)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.stream.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, string(c.model)),
	))

	options := llm.ApplyOptions(opts...)
	maxTokens := defaultMaxTokens
	if options.MaxTokens != nil {
		maxTokens = *options.MaxTokens
	}

	sys, msgParams, err := convertMessages(messages)
	if err != nil {
		span.End()
		return nil, errdefs.Validation(fmt.Errorf("anthropic: %w", err))
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
		if err := applyBetaOptions(&p, options); err != nil {
			span.End()
			return nil, err
		}
		// Same cache-anchor plumbing as the non-streaming Beta
		// branch; see the comment there.
		plan := planCacheAnchors(sys, msgParams, nil)
		applyAnchorsToBetaSystem(p.System, plan.systemBlocks)
		applyAnchorToBetaHistory(p.Messages, plan.historyMsgIdx)

		stream := c.client.Beta.Messages.NewStreaming(ctx, p)
		// Match the nil-resp guard on the non-streaming path: SDKs can
		// fail before allocating a stream handle (HTTP dial failure,
		// internal panic recovery). Without the guard, the very next
		// stream.Recv inside newBetaStreamMessage would deref nil.
		if stream == nil {
			err := errdefs.NotAvailablef("%s: nil beta stream handle (provider misbehaviour)", provider)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return nil, err
		}
		return newBetaStreamMessage(ctx, span, provider, string(c.model), stream), nil
	}

	p := asdk.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     c.model,
		Messages:  msgParams,
		System:    sys,
	}
	if err := applyOptions(&p, options); err != nil {
		span.End()
		return nil, err
	}
	plan := planCacheAnchors(p.System, p.Messages, p.Tools)
	applyAnchorsToSystem(p.System, plan.systemBlocks)
	applyAnchorToHistory(p.Messages, plan.historyMsgIdx)
	if plan.toolsLast {
		applyAnchorToTools(p.Tools)
	}

	stream := c.client.Messages.NewStreaming(ctx, p)
	if stream == nil {
		err := errdefs.NotAvailablef("%s: nil stream handle (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	return newStreamMessage(ctx, span, provider, string(c.model), stream), nil
}

// --- message conversion ---

// convertMessages translates the SDK's [llm.Message] slice into
// Anthropic's split (system, []MessageParam) shape. Multiple
// llm.Message{Role: System} entries are preserved as **independent
// text blocks** in the system slice rather than being string-joined,
// because the upstream caller convention is that each Role:System
// message represents one prompt segment that may be a cache anchor.
// The downstream automatic cache_control placement (see cache.go)
// relies on this segmentation to decide where prompt-caching
// breakpoints go; joining here would silently collapse the
// caller's intent.
//
// Tools and message-history caching are applied separately by the
// caller after convertMessages returns — they need access to the
// fully-built ToolUnionParam / MessageParam slices and are placed
// alongside the system anchors as part of the same 4-breakpoint
// budget.
func convertMessages(messages []llm.Message) (system []asdk.TextBlockParam, out []asdk.MessageParam, err error) {
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleSystem:
			// One TextBlockParam per llm.Message{Role:System}: that
			// is the cache-segmentation primitive shared with
			// callers. Empty / whitespace-only segments are dropped
			// (they would never qualify for cache_control anyway).
			if t := strings.TrimSpace(msg.Content()); t != "" {
				system = append(system, asdk.TextBlockParam{Text: t})
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
			return nil, nil, errdefs.Validationf("anthropic: unsupported role %q", msg.Role)
		}
	}

	for i := range out {
		if len(out[i].Content) == 0 {
			out[i].Content = []asdk.ContentBlockParamUnion{asdk.NewTextBlock("")}
		}
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

func applyBetaOptions(p *asdk.BetaMessageNewParams, options *llm.GenerateOptions) error {
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
	if options.Thinking != nil {
		if *options.Thinking {
			budget, err := thinkingBudgetTokens(p.MaxTokens)
			if err != nil {
				return err
			}
			p.Thinking = asdk.BetaThinkingConfigParamOfEnabled(budget)
		} else {
			disabled := asdk.NewBetaThinkingConfigDisabledParam()
			p.Thinking = asdk.BetaThinkingConfigParamUnion{OfDisabled: &disabled}
		}
	}
	return nil
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

func applyOptions(p *asdk.MessageNewParams, options *llm.GenerateOptions) error {
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
	if options.Thinking != nil {
		if *options.Thinking {
			budget, err := thinkingBudgetTokens(p.MaxTokens)
			if err != nil {
				return err
			}
			p.Thinking = asdk.ThinkingConfigParamOfEnabled(budget)
		} else {
			disabled := asdk.NewThinkingConfigDisabledParam()
			p.Thinking = asdk.ThinkingConfigParamUnion{OfDisabled: &disabled}
		}
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

	// disable_parallel_tool_use is nested inside tool_choice in the
	// Anthropic Messages API (param.Opt[bool] on Auto / Any / Tool —
	// not on None, where it would be meaningless). We bridge it from
	// opts.Extra["disable_parallel_tool_use"] (bool). When the caller
	// sets the toggle without an explicit ToolChoice, we default to
	// the Auto variant since omitted tool_choice = auto on Anthropic
	// anyway, and Auto is the only variant where the toggle composes
	// with the model's normal selection behaviour.
	disableParallel, hasDisableParallel := options.Extra["disable_parallel_tool_use"].(bool)

	if options.ToolChoice != nil {
		switch options.ToolChoice.Type {
		case llm.ToolChoiceAuto:
			tc := &asdk.ToolChoiceAutoParam{}
			if hasDisableParallel {
				tc.DisableParallelToolUse = param.NewOpt(disableParallel)
			}
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfAuto: tc}
		case llm.ToolChoiceNone:
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfNone: &asdk.ToolChoiceNoneParam{}}
		case llm.ToolChoiceRequired:
			tc := &asdk.ToolChoiceAnyParam{}
			if hasDisableParallel {
				tc.DisableParallelToolUse = param.NewOpt(disableParallel)
			}
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfAny: tc}
		case llm.ToolChoiceSpecific:
			tc := &asdk.ToolChoiceToolParam{Name: options.ToolChoice.Name}
			if hasDisableParallel {
				tc.DisableParallelToolUse = param.NewOpt(disableParallel)
			}
			p.ToolChoice = asdk.ToolChoiceUnionParam{OfTool: tc}
		}
	} else if hasDisableParallel {
		// Caller wants the toggle but didn't pick a ToolChoice — fall
		// back to Auto since that's the API default semantically.
		p.ToolChoice = asdk.ToolChoiceUnionParam{OfAuto: &asdk.ToolChoiceAutoParam{
			DisableParallelToolUse: param.NewOpt(disableParallel),
		}}
	}
	return nil
}

func thinkingBudgetTokens(maxTokens int64) (int64, error) {
	if maxTokens <= defaultThinkingBudgetTokens {
		return 0, errdefs.Validationf("anthropic: thinking requires max_tokens > %d", defaultThinkingBudgetTokens)
	}
	return defaultThinkingBudgetTokens, nil
}

func parseDataURL(s string) (mediaType, base64Data string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "data:") {
		return "", "", errdefs.Validationf("expected data url")
	}
	rest := strings.TrimPrefix(s, "data:")
	i := strings.Index(rest, ";base64,")
	if i <= 0 {
		return "", "", errdefs.Validationf("invalid data url")
	}
	return rest[:i], rest[i+len(";base64,"):], nil
}
