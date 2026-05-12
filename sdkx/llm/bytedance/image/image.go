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
	defaultBaseURL = "https://ark.cn-beijing.volces.com"
	imageGenPath   = "/api/v3/images/generations"
	defaultModel   = "doubao-seedream-5-0-260128"
	defaultTimeout = 120 * time.Second

	// providerKey is the registry key. Kept distinct from the
	// "bytedance" chat provider so credentials and rate-limit
	// pools can be configured independently — even when both use
	// the same Volc Ark API key, they hit different SKUs.
	providerKey = "bytedance-image"

	// maxRefsPlusOutputs caps refs+outputs at 15 per Seedream's
	// documented limit ("输入的参考图数量 + 最终生成的图片数量 ≤ 15").
	maxRefsPlusOutputs = 15
)

func init() {
	llm.RegisterProvider(providerKey, func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects Seedream's image-generation lineup as of
	// 2026-04-30. Sources:
	//   - https://www.volcengine.com/docs/82379/1824121
	//   - https://console.volcengine.com/ark/region:ark+cn-beijing/model/detail
	//
	// Caps: every chat-completion knob is disabled (Seedream is a
	// dedicated image-gen surface that does not honour temperature,
	// tools-from-LLM, JSON mode, system prompts, etc.). Streaming
	// IS supported natively by the endpoint, but the StreamMessage
	// contract (text-oriented StreamChunk) cannot express image
	// payloads, so we disable CapStreaming and rely on the caps
	// middleware's Generate→one-chunk downgrade — same trade-off
	// as the minimax-image adapter.
	imageOnly := llm.DisabledCaps(
		llm.CapTemperature, llm.CapTopP, llm.CapTopK, llm.CapMaxTokens,
		llm.CapStopWords, llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		llm.CapThinking,
		llm.CapJSONMode, llm.CapJSONSchema,
		llm.CapTools, llm.CapToolChoice, llm.CapParallelTools,
		llm.CapStreaming, llm.CapSystemPrompt,
		llm.CapAudio, llm.CapFile,
		llm.CapAudioOutput,
		// CapVision and CapImageOutput remain ENABLED.
	)

	llm.RegisterProviderModels(providerKey, []llm.ModelInfo{
		// --- Seedream 5.0 lite (current default) ---------------------
		// Two model IDs route to the same SKU per the upstream doc:
		// "doubao-seedream-5-0-260128 (同时支持：doubao-seedream-5-0-lite-260128)".
		// Both are registered so callers can pin either.
		{
			Label: "Doubao Seedream 5.0 lite",
			Name:  "doubao-seedream-5-0-260128",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
		{
			Label: "Doubao Seedream 5.0 lite (alias)",
			Name:  "doubao-seedream-5-0-lite-260128",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
		// --- Seedream 4.5 -------------------------------------------
		{
			Label: "Doubao Seedream 4.5",
			Name:  "doubao-seedream-4-5-251128",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
		// --- Seedream 4.0 -------------------------------------------
		// Only SKU that supports the optimize_prompt_options.mode=fast
		// fast-path; expose via llm.WithExtra("optimize_prompt_mode",
		// "fast") on a per-call basis.
		{
			Label: "Doubao Seedream 4.0",
			Name:  "doubao-seedream-4-0-250828",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
	})
}

// LLM is the Seedream image-generation adapter. It implements
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

// New builds an adapter pointing at the Seedream endpoint. An empty
// model defaults to "doubao-seedream-5-0-260128"; an empty baseURL
// defaults to https://ark.cn-beijing.volces.com.
func New(modelName, apiKey, baseURL string, opts ...Option) (*LLM, error) {
	if apiKey == "" {
		return nil, errdefs.Validation(errdefs.New("bytedance-image: api_key required"))
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

// Generate calls Seedream /api/v3/images/generations and returns an
// assistant message whose Parts are PartImage entries (one per
// generated image). TokenUsage is populated from the response when
// present; Seedream reports image-count usage rather than token
// counts.
func (l *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx,
		fmt.Sprintf("llm.bytedance-image.generate.%s", l.model),
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
	prompt, refs := extractPromptAndRefs(messages)
	if prompt == "" {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(errdefs.New("bytedance-image: empty prompt (no text parts in user messages)"))
	}

	body, err := buildRequest(l.model, prompt, refs, o.ImageGen, o.Extra)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	resp, err := l.do(ctx, body)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	// Family-wide nil-resp guard — same rationale as the openai /
	// anthropic / bytedance fixes. Cheap insurance against a future
	// do() refactor that drops an error branch.
	if resp == nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.NotAvailable(errdefs.New("bytedance-image: nil response with no error (provider misbehaviour)"))
	}
	parts, err := imagesToParts(resp.Data, body.OutputFormat)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	if len(parts) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Internal(errdefs.New("bytedance-image: provider returned zero images"))
	}
	usage := llm.TokenUsage{Model: l.model}
	if resp.Usage != nil {
		usage.OutputTokens = int64(resp.Usage.GeneratedImages)
	}
	return llm.Message{Role: model.RoleAssistant, Parts: parts}, usage, nil
}

// GenerateStream wraps Generate via [llm.NewOneChunkStream]. Native
// streaming on this endpoint emits image-bearing events that the
// text-oriented [llm.StreamChunk] envelope cannot represent; see
// package doc for rationale.
func (l *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := l.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return llm.NewOneChunkStream(msg, usage), nil
}

// --- request / response wire shape -----------------------------------

type apiRequest struct {
	Model                            string        `json:"model"`
	Prompt                           string        `json:"prompt"`
	Image                            any           `json:"image,omitempty"` // string for 1, []string for >1
	Size                             string        `json:"size,omitempty"`
	OutputFormat                     string        `json:"output_format,omitempty"`
	ResponseFormat                   string        `json:"response_format,omitempty"`
	Watermark                        *bool         `json:"watermark,omitempty"`
	SequentialImageGeneration        string        `json:"sequential_image_generation,omitempty"`
	SequentialImageGenerationOptions *seqOpts      `json:"sequential_image_generation_options,omitempty"`
	OptimizePromptOptions            *optimizeOpts `json:"optimize_prompt_options,omitempty"`
	Tools                            []toolSpec    `json:"tools,omitempty"`
}

type seqOpts struct {
	MaxImages int `json:"max_images"`
}

type optimizeOpts struct {
	Mode string `json:"mode"`
}

type toolSpec struct {
	Type string `json:"type"`
}

type apiResponse struct {
	Model string         `json:"model,omitempty"`
	Data  []responseItem `json:"data"`
	Usage *apiUsage      `json:"usage,omitempty"`
	Error *apiError      `json:"error,omitempty"`
}

type responseItem struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
	Size    string `json:"size,omitempty"`
}

type apiUsage struct {
	GeneratedImages int `json:"generated_images,omitempty"`
}

type apiError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func buildRequest(modelName, prompt string, refs []string, ig *llm.ImageGenOptions, extra map[string]any) (apiRequest, error) {
	req := apiRequest{Model: modelName, Prompt: prompt}

	switch len(refs) {
	case 0:
		// text-to-image
	case 1:
		req.Image = refs[0]
	default:
		req.Image = refs
	}

	if ig != nil {
		// Size: pixel form takes priority. AspectRatio and Seed have
		// no native Seedream knob; document and ignore.
		if ig.Width > 0 && ig.Height > 0 {
			req.Size = fmt.Sprintf("%dx%d", ig.Width, ig.Height)
		}
		switch ig.ResponseFormat {
		case llm.ResponseFormatURL:
			req.ResponseFormat = "url"
		case llm.ResponseFormatBase64:
			req.ResponseFormat = "b64_json"
		}
		if ig.N > 1 {
			if ig.N+len(refs) > maxRefsPlusOutputs {
				return req, errdefs.Validation(errdefs.Fmt("bytedance-image: refs(%d) + N(%d) exceeds Seedream cap of %d", len(refs), ig.N, maxRefsPlusOutputs))
			}
			req.SequentialImageGeneration = "auto"
			req.SequentialImageGenerationOptions = &seqOpts{MaxImages: ig.N}
		}
	}

	// Extra escape hatches for provider-specific knobs that don't
	// belong on the public ImageGenOptions surface.
	if v, ok := stringExtra(extra, "size"); ok && v != "" {
		req.Size = v // overrides W/H-derived size
	}
	if v, ok := stringExtra(extra, "output_format"); ok && v != "" {
		req.OutputFormat = v
	}
	if v, ok := boolExtra(extra, "watermark"); ok {
		req.Watermark = &v
	}
	if v, ok := stringExtra(extra, "optimize_prompt_mode"); ok && v != "" {
		req.OptimizePromptOptions = &optimizeOpts{Mode: v}
	}
	if v, ok := boolExtra(extra, "web_search"); ok && v {
		req.Tools = append(req.Tools, toolSpec{Type: "web_search"})
	}

	return req, nil
}

func extractPromptAndRefs(messages []llm.Message) (prompt string, refs []string) {
	var lastUserText strings.Builder
	for _, m := range messages {
		if m.Role != model.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case model.PartImage:
				if p.Image != nil && p.Image.URL != "" {
					refs = append(refs, p.Image.URL)
				}
			case model.PartText:
				if lastUserText.Len() > 0 {
					lastUserText.WriteString("\n")
				}
				lastUserText.WriteString(p.Text)
			}
		}
	}
	return strings.TrimSpace(lastUserText.String()), refs
}

func imagesToParts(items []responseItem, outputFormat string) ([]model.Part, error) {
	mediaType := "image/jpeg" // Seedream default for 4.0 / 4.5
	if strings.EqualFold(outputFormat, "png") {
		mediaType = "image/png"
	}
	parts := make([]model.Part, 0, len(items))
	for _, it := range items {
		if it.URL != "" {
			parts = append(parts, model.Part{Type: model.PartImage, Image: &model.MediaRef{URL: it.URL, MediaType: mediaType}})
			continue
		}
		if it.B64JSON != "" {
			parts = append(parts, model.Part{Type: model.PartImage, Image: &model.MediaRef{Base64: it.B64JSON, MediaType: mediaType}})
		}
	}
	return parts, nil
}

func (l *LLM) do(ctx context.Context, req apiRequest) (*apiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("bytedance-image: marshal request: %w", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+imageGenPath, bytes.NewReader(body))
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("bytedance-image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+l.apiKey)

	resp, err := l.client.Do(httpReq)
	if err != nil {
		return nil, errdefs.NotAvailable(errdefs.Fmt("bytedance-image: http call: %w", err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("bytedance-image: read body: %w", err))
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errdefs.Unauthorized(errdefs.Fmt("bytedance-image: 401: %s", truncate(string(raw), 200)))
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errdefs.RateLimit(errdefs.Fmt("bytedance-image: 429: %s", truncate(string(raw), 200)))
	case resp.StatusCode >= 500:
		return nil, errdefs.NotAvailable(errdefs.Fmt("bytedance-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	case resp.StatusCode >= 400:
		return nil, errdefs.Validation(errdefs.Fmt("bytedance-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("bytedance-image: decode body: %w (raw=%s)", err, truncate(string(raw), 200)))
	}
	if out.Error != nil && out.Error.Code != "" {
		return nil, mapAPIError(out.Error)
	}
	return &out, nil
}

// mapAPIError maps Volc Ark error codes to errdefs categories. The
// Ark error envelope reuses the Volcengine "TopErrorCode" naming
// convention; the cases below cover the documented ImageGenerations
// failures (see /docs/82379/1330310 — model list and limits).
func mapAPIError(e *apiError) error {
	msg := fmt.Sprintf("bytedance-image: %s %s", e.Code, e.Message)
	switch {
	case strings.HasPrefix(e.Code, "AuthenticationError"),
		strings.HasPrefix(e.Code, "InvalidAccessKey"),
		strings.HasPrefix(e.Code, "MissingAuthHeader"):
		return errdefs.Unauthorized(errdefs.New(msg))
	case strings.HasPrefix(e.Code, "RateLimitExceeded"),
		strings.HasPrefix(e.Code, "QuotaExceeded"):
		return errdefs.RateLimit(errdefs.New(msg))
	case strings.HasPrefix(e.Code, "InsufficientBalance"),
		strings.HasPrefix(e.Code, "AccessDenied"):
		return errdefs.Forbidden(errdefs.New(msg))
	case strings.HasPrefix(e.Code, "InvalidParameter"),
		strings.HasPrefix(e.Code, "InvalidRequest"),
		strings.HasPrefix(e.Code, "ResourceNotFound"):
		return errdefs.Validation(errdefs.New(msg))
	case strings.HasPrefix(e.Code, "InternalServiceError"),
		strings.HasPrefix(e.Code, "ServiceUnavailable"):
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
