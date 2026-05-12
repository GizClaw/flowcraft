package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// defaultBaseURL is the Beijing endpoint. The international
	// (Singapore) endpoint (https://dashscope-intl.aliyuncs.com) is
	// reachable by overriding base_url at config time. Beijing and
	// Singapore have independent API keys and are not cross-callable.
	defaultBaseURL = "https://dashscope.aliyuncs.com"
	imageGenPath   = "/api/v1/services/aigc/multimodal-generation/generation"
	defaultModel   = "qwen-image-2.0-pro"
	defaultTimeout = 120 * time.Second

	providerKey = "qwen-image"
)

func init() {
	llm.RegisterProvider(providerKey, func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects DashScope's Qwen-Image lineup as of
	// 2026-04-30. Sources:
	//   - https://help.aliyun.com/zh/model-studio/qwen-image-api
	//   - https://help.aliyun.com/zh/model-studio/text-to-image
	//
	// Caps: every chat-completion knob is disabled. CapVision stays
	// disabled because this endpoint is text-to-image ONLY — image
	// editing is on a separate qwen-image-edit endpoint and would be
	// served by a future adapter. Audio output stays disabled
	// because Qwen-Image emits images only.
	imageOnly := llm.DisabledCaps(
		llm.CapTemperature, llm.CapTopP, llm.CapTopK, llm.CapMaxTokens,
		llm.CapStopWords, llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		llm.CapThinking,
		llm.CapJSONMode, llm.CapJSONSchema,
		llm.CapTools, llm.CapToolChoice, llm.CapParallelTools,
		llm.CapStreaming, llm.CapSystemPrompt,
		// Input modalities: this endpoint is text-only.
		llm.CapVision, llm.CapAudio, llm.CapFile,
		// Output modalities: image only.
		llm.CapAudioOutput,
		// CapImageOutput remains ENABLED.
	)

	llm.RegisterProviderModels(providerKey, []llm.ModelInfo{
		// --- 2.0 Pro series (current recommended flagship) -----------
		// Total pixels in [512*512, 2048*2048], default 2048*2048.
		// n: 1-6.
		{Label: "Qwen-Image 2.0 Pro", Name: "qwen-image-2.0-pro", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image 2.0 Pro (2026-04-22)", Name: "qwen-image-2.0-pro-2026-04-22", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image 2.0 Pro (2026-03-03)", Name: "qwen-image-2.0-pro-2026-03-03", Spec: llm.ModelSpec{Caps: imageOnly}},

		// --- 2.0 series (accelerated, recommended) ------------------
		{Label: "Qwen-Image 2.0", Name: "qwen-image-2.0", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image 2.0 (2026-03-03)", Name: "qwen-image-2.0-2026-03-03", Spec: llm.ModelSpec{Caps: imageOnly}},

		// --- Max / Plus series --------------------------------------
		// Default 1664*928 (16:9). n: fixed at 1; server errors on
		// other values.
		{Label: "Qwen-Image Max", Name: "qwen-image-max", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image Max (2025-12-30)", Name: "qwen-image-max-2025-12-30", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image Plus", Name: "qwen-image-plus", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image Plus (2026-01-09)", Name: "qwen-image-plus-2026-01-09", Spec: llm.ModelSpec{Caps: imageOnly}},
		{Label: "Qwen-Image", Name: "qwen-image", Spec: llm.ModelSpec{Caps: imageOnly}},
	})
}

// LLM is the Qwen-Image text-to-image adapter. It implements
// [llm.LLM] so callers can route image generation through the same
// resolver and fallback machinery as chat models.
type LLM struct {
	model   string
	apiKey  string
	baseURL string
	client  *http.Client
}

// Option configures the adapter at construction time.
type Option func(*LLM)

// WithHTTPClient overrides the default *http.Client (120s timeout).
// Useful for tests and for plugging in OTel/transport middleware.
func WithHTTPClient(c *http.Client) Option {
	return func(l *LLM) {
		if c != nil {
			l.client = c
		}
	}
}

// New builds an adapter pointing at the DashScope Qwen-Image
// endpoint. An empty model defaults to "qwen-image-2.0-pro"; an
// empty baseURL defaults to https://dashscope.aliyuncs.com (Beijing).
// Set baseURL to "https://dashscope-intl.aliyuncs.com" for the
// international (Singapore) region.
func New(modelName, apiKey, baseURL string, opts ...Option) (*LLM, error) {
	if apiKey == "" {
		return nil, errdefs.Validation(errdefs.New("qwen-image: api_key required"))
	}
	if modelName == "" {
		modelName = defaultModel
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	l := &LLM{
		model:   modelName,
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// --- llm.LLM ----------------------------------------------------------

// Generate calls the Qwen-Image multimodal-generation endpoint and
// returns an assistant message whose Parts are PartImage entries
// (one per generated image). TokenUsage is left zero — the endpoint
// reports image_count rather than token counts.
func (l *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx,
		fmt.Sprintf("llm.qwen-image.generate.%s", l.model),
		trace.WithAttributes(
			attribute.String(telemetry.AttrLLMProvider, providerKey),
			attribute.String(telemetry.AttrLLMModel, l.model),
		))
	defer span.End()

	start := time.Now()
	msg, usage, err := l.generate(ctx, messages, opts...)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, providerKey, l.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}
	span.SetAttributes(attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens))
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, providerKey, l.model, "success", dur, usage)
	return msg, usage, nil
}

func (l *LLM) generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	o := llm.ApplyOptions(opts...)
	prompt, refsCount := extractPrompt(messages)
	if prompt == "" {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(errdefs.New("qwen-image: empty prompt (no text parts in user messages)"))
	}
	if refsCount > 0 {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(errdefs.New("qwen-image: this endpoint is text-to-image only; image editing is on the separate qwen-image-edit endpoint (not yet adapted)"))
	}

	body := buildRequest(l.model, prompt, o.ImageGen, o.Extra)
	resp, err := l.do(ctx, body)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	// Defensive nil-resp guard. The hand-rolled do() currently
	// returns (nil, err) on every error path, but the contract is
	// load-bearing for safety: a future refactor that accidentally
	// drops an error branch would otherwise crash the caller at
	// the next deref. Matches the family-wide guard added to
	// sdkx/llm/{openai,anthropic,bytedance}.
	if resp == nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.NotAvailable(errdefs.New("qwen-image: nil response with no error (provider misbehaviour)"))
	}
	parts, err := imagesToParts(resp)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	if len(parts) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Internal(errdefs.New("qwen-image: provider returned zero images"))
	}
	usage := llm.TokenUsage{Model: l.model}
	if resp.Usage != nil {
		usage.OutputTokens = int64(resp.Usage.ImageCount)
	}
	return llm.Message{Role: model.RoleAssistant, Parts: parts}, usage, nil
}

// GenerateStream wraps Generate via [llm.NewOneChunkStream]; the
// endpoint is synchronous.
func (l *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := l.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return llm.NewOneChunkStream(msg, usage), nil
}

// --- request / response wire shape -----------------------------------

type apiRequest struct {
	Model      string        `json:"model"`
	Input      apiInput      `json:"input"`
	Parameters apiParameters `json:"parameters,omitempty"`
}

type apiInput struct {
	Messages []apiMessage `json:"messages"`
}

type apiMessage struct {
	Role    string       `json:"role"`
	Content []apiContent `json:"content"`
}

type apiContent struct {
	Text string `json:"text"`
}

type apiParameters struct {
	NegativePrompt string `json:"negative_prompt,omitempty"`
	Size           string `json:"size,omitempty"`
	N              int    `json:"n,omitempty"`
	PromptExtend   *bool  `json:"prompt_extend,omitempty"`
	Watermark      *bool  `json:"watermark,omitempty"`
	Seed           *int64 `json:"seed,omitempty"`
}

type apiResponse struct {
	RequestID string     `json:"request_id,omitempty"`
	Code      string     `json:"code,omitempty"`
	Message   string     `json:"message,omitempty"`
	Output    *apiOutput `json:"output,omitempty"`
	Usage     *apiUsage  `json:"usage,omitempty"`
}

type apiOutput struct {
	Choices []apiChoice `json:"choices,omitempty"`
}

type apiChoice struct {
	FinishReason string         `json:"finish_reason,omitempty"`
	Message      *apiOutMessage `json:"message,omitempty"`
}

type apiOutMessage struct {
	Role    string              `json:"role,omitempty"`
	Content []apiOutContentItem `json:"content,omitempty"`
}

type apiOutContentItem struct {
	Image string `json:"image,omitempty"`
	Text  string `json:"text,omitempty"`
}

type apiUsage struct {
	ImageCount int `json:"image_count,omitempty"`
	Width      int `json:"width,omitempty"`
	Height     int `json:"height,omitempty"`
}

func buildRequest(modelName, prompt string, ig *llm.ImageGenOptions, extra map[string]any) apiRequest {
	req := apiRequest{
		Model: modelName,
		Input: apiInput{Messages: []apiMessage{{
			Role:    "user",
			Content: []apiContent{{Text: prompt}},
		}}},
	}

	if ig != nil {
		// Note the unusual "*" separator: Qwen uses "W*H", not the
		// "WxH" common to OpenAI / Seedream / MiniMax.
		if ig.Width > 0 && ig.Height > 0 {
			req.Parameters.Size = fmt.Sprintf("%d*%d", ig.Width, ig.Height)
		}
		if ig.N > 0 {
			req.Parameters.N = ig.N
		}
		if ig.Seed != nil {
			s := *ig.Seed
			req.Parameters.Seed = &s
		}
		// AspectRatio and ResponseFormat have no native knob on this
		// endpoint and are silently ignored — see package doc.
	}

	// Provider-specific overrides via Extra.
	if v, ok := stringExtra(extra, "negative_prompt"); ok {
		req.Parameters.NegativePrompt = v
	}
	if v, ok := stringExtra(extra, "size"); ok && v != "" {
		req.Parameters.Size = v // overrides W*H-derived size
	}
	if v, ok := boolExtra(extra, "prompt_extend"); ok {
		req.Parameters.PromptExtend = &v
	}
	if v, ok := boolExtra(extra, "watermark"); ok {
		req.Parameters.Watermark = &v
	}

	return req
}

// extractPrompt collects all user-message text into one prompt and
// reports the count of [model.PartImage] inputs so the caller can
// fail-fast on an attempt to use this t2i-only endpoint with image
// references.
func extractPrompt(messages []llm.Message) (prompt string, imageRefs int) {
	var b strings.Builder
	for _, m := range messages {
		if m.Role != model.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case model.PartText:
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			case model.PartImage:
				imageRefs++
			}
		}
	}
	return strings.TrimSpace(b.String()), imageRefs
}

func imagesToParts(resp *apiResponse) ([]model.Part, error) {
	if resp.Output == nil || len(resp.Output.Choices) == 0 {
		return nil, nil
	}
	choice := resp.Output.Choices[0]
	if choice.Message == nil {
		return nil, nil
	}
	parts := make([]model.Part, 0, len(choice.Message.Content))
	for _, c := range choice.Message.Content {
		if c.Image == "" {
			continue
		}
		parts = append(parts, model.Part{
			Type:  model.PartImage,
			Image: &model.MediaRef{URL: c.Image, MediaType: "image/png"},
		})
	}
	return parts, nil
}

func (l *LLM) do(ctx context.Context, req apiRequest) (*apiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("qwen-image: marshal request: %w", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+imageGenPath, bytes.NewReader(body))
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("qwen-image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+l.apiKey)

	resp, err := l.client.Do(httpReq)
	if err != nil {
		return nil, errdefs.NotAvailable(errdefs.Fmt("qwen-image: http call: %w", err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("qwen-image: read body: %w", err))
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errdefs.Unauthorized(errdefs.Fmt("qwen-image: 401: %s", truncate(string(raw), 200)))
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errdefs.RateLimit(errdefs.Fmt("qwen-image: 429: %s", truncate(string(raw), 200)))
	case resp.StatusCode >= 500:
		return nil, errdefs.NotAvailable(errdefs.Fmt("qwen-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	case resp.StatusCode >= 400:
		// 400 may carry a structured error envelope — try to decode
		// it for a more helpful category.
		if err := tryDecodeError(raw); err != nil {
			return nil, err
		}
		return nil, errdefs.Validation(errdefs.Fmt("qwen-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("qwen-image: decode body: %w (raw=%s)", err, truncate(string(raw), 200)))
	}
	// 200-but-business-error: code is set on top-level when the
	// model rejects the request (e.g. "InvalidParameter").
	if out.Code != "" {
		return nil, mapAPIError(out.Code, out.Message)
	}
	return &out, nil
}

func tryDecodeError(raw []byte) error {
	var env struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == "" {
		return nil
	}
	return mapAPIError(env.Code, env.Message)
}

// mapAPIError maps DashScope error codes to errdefs categories. The
// DashScope error envelope follows the Aliyun convention; the cases
// below cover documented Qwen-Image failures (see
// help.aliyun.com/zh/model-studio/error-code).
func mapAPIError(code, message string) error {
	msg := fmt.Sprintf("qwen-image: %s %s", code, message)
	switch {
	case strings.HasPrefix(code, "InvalidApiKey"),
		strings.HasPrefix(code, "AuthenticationError"),
		strings.HasPrefix(code, "Unauthorized"):
		return errdefs.Unauthorized(errdefs.New(msg))
	case strings.HasPrefix(code, "Throttling"),
		strings.HasPrefix(code, "RateLimit"),
		strings.HasPrefix(code, "RequestLimitExceeded"):
		return errdefs.RateLimit(errdefs.New(msg))
	case strings.HasPrefix(code, "AccessDenied"),
		strings.HasPrefix(code, "ArrearagedAccount"),
		strings.HasPrefix(code, "InsufficientBalance"):
		return errdefs.Forbidden(errdefs.New(msg))
	case strings.HasPrefix(code, "InvalidParameter"),
		strings.HasPrefix(code, "InvalidRequest"),
		strings.HasPrefix(code, "ResourceNotFound"),
		strings.HasPrefix(code, "ContentFiltered"),
		strings.HasPrefix(code, "DataInspection"):
		return errdefs.Validation(errdefs.New(msg))
	case strings.HasPrefix(code, "InternalError"),
		strings.HasPrefix(code, "ServiceUnavailable"):
		return errdefs.NotAvailable(errdefs.New(msg))
	default:
		return errdefs.Internal(errdefs.New(msg))
	}
}

// --- helpers ---------------------------------------------------------

func stringExtra(extra map[string]any, key string) (string, bool) {
	if extra == nil {
		return "", false
	}
	v, ok := extra[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolExtra(extra map[string]any, key string) (bool, bool) {
	if extra == nil {
		return false, false
	}
	v, ok := extra[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
