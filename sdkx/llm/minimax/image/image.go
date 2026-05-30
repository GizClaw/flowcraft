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
	defaultBaseURL = "https://api.minimaxi.com"
	imageGenPath   = "/v1/image_generation"
	defaultModel   = "image-01"
	maxPromptChars = 1500
	defaultTimeout = 90 * time.Second
)

const providerKey = "minimax-image"

func init() {
	llm.RegisterProvider(providerKey, func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects MiniMax's image-gen lineup as of 2026-04-30.
	// Sources:
	//   - https://platform.minimax.io/docs/api-reference/image-generation-i2i
	//   - https://platform.minimax.io/docs/guides/image-generation
	//
	// Both image-01 SKUs are dedicated image-generation models served
	// out of /v1/image_generation; they do not honour any of the
	// chat-completion knobs (temperature, tools, JSON mode, …) and
	// cannot stream natively. Caps below disable everything that does
	// not apply so the caps middleware fails fast at the SDK edge.
	imageOnly := llm.DisabledCaps(
		// Generation params (chat-only).
		llm.CapTemperature, llm.CapTopP, llm.CapTopK, llm.CapMaxTokens,
		llm.CapStopWords, llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		llm.CapThinking,
		// Output controls (chat-only).
		llm.CapJSONMode, llm.CapJSONSchema,
		// Protocol features (chat-only).
		llm.CapTools, llm.CapToolChoice, llm.CapParallelTools,
		llm.CapStreaming, llm.CapSystemPrompt,
		// Input modalities not accepted by /v1/image_generation.
		llm.CapAudio, llm.CapFile,
		// Output modalities — the model emits images, not audio.
		llm.CapAudioOutput,
		// CapVision and CapImageOutput remain ENABLED so the resolver
		// can route image-output policy slots to this adapter while
		// the caps middleware permits image input parts.
	)

	llm.RegisterProviderModels(providerKey, []llm.ModelInfo{
		{
			Label: "MiniMax Image-01",
			Name:  "image-01",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
		{
			Label: "MiniMax Image-01 Live",
			Name:  "image-01-live",
			Spec:  llm.ModelSpec{Caps: imageOnly},
		},
	})
}

// LLM is the MiniMax image-generation adapter. It implements
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

// WithHTTPClient overrides the default *http.Client (90s timeout).
// Useful for tests and for plugging in OTel/transport middleware.
func WithHTTPClient(c *http.Client) Option {
	return func(l *LLM) {
		if c != nil {
			l.client = c
		}
	}
}

// New builds an adapter pointing at MiniMax's image-generation
// endpoint. An empty model defaults to "image-01"; an empty baseURL
// defaults to https://api.minimaxi.com.
func New(modelName, apiKey, baseURL string, opts ...Option) (*LLM, error) {
	if apiKey == "" {
		return nil, errdefs.Validation(errdefs.New("minimax-image: api_key required"))
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

// Generate calls MiniMax /v1/image_generation and returns an assistant
// message whose Parts are PartImage entries (one per generated image).
// The TokenUsage is always zero — the endpoint does not report usage.
func (l *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx,
		fmt.Sprintf("llm.minimax-image.generate.%s", l.model),
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
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, providerKey, l.model, "success", dur, usage)
	return msg, usage, nil
}

func (l *LLM) generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	o := llm.ApplyOptions(opts...)
	prompt, refs := extractPromptAndRefs(messages)
	if prompt == "" {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(errdefs.New("minimax-image: empty prompt (no text parts in user messages)"))
	}
	if cnt := len([]rune(prompt)); cnt > maxPromptChars {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(errdefs.Fmt("minimax-image: prompt is %d chars (max %d)", cnt, maxPromptChars))
	}

	body := buildRequest(l.model, prompt, refs, o.ImageGen, o.Extra)
	resp, err := l.do(ctx, body)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	// Family-wide nil-resp guard — same rationale as the openai /
	// anthropic / bytedance fixes. Cheap insurance against a future
	// do() refactor that drops an error branch.
	if resp == nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.NotAvailable(errdefs.New("minimax-image: nil response with no error (provider misbehaviour)"))
	}
	if resp.BaseResp.StatusCode != 0 {
		return llm.Message{}, llm.TokenUsage{}, mapAPIError(resp.BaseResp)
	}
	parts, err := imagesToParts(resp.Data)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	if len(parts) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Internal(errdefs.New("minimax-image: provider returned zero images"))
	}
	return llm.Message{Role: model.RoleAssistant, Parts: parts}, llm.TokenUsage{}, nil
}

// GenerateStream wraps Generate via [llm.NewOneChunkStream] because
// /v1/image_generation is synchronous. The returned stream emits a
// single chunk carrying the assistant role and "stop" finish reason;
// callers retrieve the full multimodal payload (image Parts) from
// Message() after iteration completes — [llm.StreamChunk] is text-
// oriented and has no Parts field.
func (l *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := l.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return llm.NewOneChunkStream(msg, usage), nil
}

// --- request / response wire shape -----------------------------------

type apiRequest struct {
	Model            string             `json:"model"`
	Prompt           string             `json:"prompt"`
	SubjectReference []subjectReference `json:"subject_reference,omitempty"`
	AspectRatio      string             `json:"aspect_ratio,omitempty"`
	Width            int                `json:"width,omitempty"`
	Height           int                `json:"height,omitempty"`
	N                int                `json:"n,omitempty"`
	Seed             *int64             `json:"seed,omitempty"`
	ResponseFormat   string             `json:"response_format,omitempty"`
}

type subjectReference struct {
	Type      string `json:"type"`
	ImageFile string `json:"image_file"`
}

type apiResponse struct {
	ID       string       `json:"id,omitempty"`
	Data     responseData `json:"data"`
	Metadata any          `json:"metadata,omitempty"`
	BaseResp baseResp     `json:"base_resp"`
}

type responseData struct {
	ImageURLs    []string `json:"image_urls,omitempty"`
	ImageBase64s []string `json:"image_base64,omitempty"`
}

type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

func buildRequest(modelName, prompt string, refs []string, opts *llm.ImageGenOptions, extra map[string]any) apiRequest {
	req := apiRequest{Model: modelName, Prompt: prompt}
	for _, url := range refs {
		req.SubjectReference = append(req.SubjectReference, subjectReference{
			Type:      "character",
			ImageFile: url,
		})
	}
	if opts != nil {
		req.AspectRatio = opts.AspectRatio
		req.Width = opts.Width
		req.Height = opts.Height
		req.N = opts.N
		req.Seed = opts.Seed
		switch opts.ResponseFormat {
		case llm.ResponseFormatBase64:
			req.ResponseFormat = "base64"
		case llm.ResponseFormatURL:
			req.ResponseFormat = "url"
		}
	}
	_ = extra // reserved for future provider-specific overrides
	return req
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

func imagesToParts(d responseData) ([]model.Part, error) {
	parts := make([]model.Part, 0, len(d.ImageURLs)+len(d.ImageBase64s))
	for _, u := range d.ImageURLs {
		parts = append(parts, model.Part{Type: model.PartImage, Image: &model.MediaRef{URL: u, MediaType: "image/png"}})
	}
	for _, b := range d.ImageBase64s {
		parts = append(parts, model.Part{Type: model.PartImage, Image: &model.MediaRef{Base64: b, MediaType: "image/png"}})
	}
	return parts, nil
}

func (l *LLM) do(ctx context.Context, req apiRequest) (*apiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("minimax-image: marshal request: %w", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+imageGenPath, bytes.NewReader(body))
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("minimax-image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+l.apiKey)

	resp, err := l.client.Do(httpReq)
	if err != nil {
		return nil, errdefs.NotAvailable(errdefs.Fmt("minimax-image: http call: %w", err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("minimax-image: read body: %w", err))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errdefs.Unauthorized(errdefs.Fmt("minimax-image: 401: %s", truncate(string(raw), 200)))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errdefs.RateLimit(errdefs.Fmt("minimax-image: 429: %s", truncate(string(raw), 200)))
	}
	if resp.StatusCode >= 500 {
		return nil, errdefs.NotAvailable(errdefs.Fmt("minimax-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	}
	if resp.StatusCode >= 400 {
		return nil, errdefs.Validation(errdefs.Fmt("minimax-image: %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	}
	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errdefs.Internal(errdefs.Fmt("minimax-image: decode body: %w (raw=%s)", err, truncate(string(raw), 200)))
	}
	return &out, nil
}

// mapAPIError maps MiniMax base_resp non-zero codes to errdefs categories.
// Documented codes: 1000-series (auth/quota), 1004 (rate-limit), 1008
// (insufficient balance), 2013 (invalid params), etc.
func mapAPIError(b baseResp) error {
	msg := fmt.Sprintf("minimax-image: %d %s", b.StatusCode, b.StatusMsg)
	switch b.StatusCode {
	case 1000, 1001, 1002, 1004:
		return errdefs.RateLimit(errdefs.New(msg))
	case 1008, 1013:
		return errdefs.Forbidden(errdefs.New(msg))
	case 1003, 1005, 1006:
		return errdefs.Unauthorized(errdefs.New(msg))
	case 2013, 2049:
		return errdefs.Validation(errdefs.New(msg))
	default:
		return errdefs.Internal(errdefs.New(msg))
	}
}

// --- helpers ---------------------------------------------------------

func truncate(s string, n int) string {
	runes := []rune(s)
	if n <= 0 || len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
