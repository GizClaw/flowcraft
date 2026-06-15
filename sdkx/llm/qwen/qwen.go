package qwen

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

func init() {
	llm.RegisterProvider("qwen", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects Alibaba DashScope's Qwen commercial lineup as
	// of 2026-04-30. Sources:
	//   - https://www.alibabacloud.com/help/en/model-studio/models
	//   - https://cloudprice.net/models/alibaba-qwen-plus
	//   - https://www.appaca.ai/resources/llm-comparison/qwen-flash-vs-qwen-turbo
	//
	// Caveat on context window: the published architectural ceiling
	// (e.g. 1M for Plus / Flash / Turbo) is not always the effective
	// per-request limit on every DashScope plan; coding-tier accounts
	// have been reported to cap requests around 170K. We declare the
	// architectural ceiling as MaxContextTokens (informational) and
	// rely on the API surface to error if the deployment plan is
	// stricter.
	//
	// Audio / file modalities go through dedicated DashScope endpoints
	// (qwen-audio, qwen-omni) — disable both at the caps middleware
	// for these chat-completions models so callers get a fail-fast
	// rather than a backend error. Vision is supported across the
	// commercial chat models (qwen-max et al.) per Model Studio docs.
	//
	// Output modality: text only. Image generation lives in the
	// dedicated Qwen-Image SKU and audio output in Qwen3-Omni /
	// Qwen3.5-Omni — separate adapters not catalogued here. Disable
	// the matching output caps so policy matching does not route
	// image-output / audio-output slots onto these chat models.
	qwenChatCaps := llm.DisabledCaps(
		llm.CapAudio, llm.CapFile,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	qwenModels := []llm.ModelInfo{
		{
			// Flagship; 256K context per Model Studio docs.
			Label: "Qwen Max",
			Name:  "qwen-max",
			Spec: llm.ModelSpec{
				Caps:   qwenChatCaps,
				Limits: llm.ModelLimits{MaxContextTokens: 262_144},
			},
		},
		{
			// Balanced; 1M context, 33K max output per CloudPrice listing.
			Label: "Qwen Plus",
			Name:  "qwen-plus",
			Spec: llm.ModelSpec{
				Caps: qwenChatCaps,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  33_000,
				},
			},
		},
		{
			// 1M context per Appaca comparison; max output not stated.
			Label: "Qwen Turbo",
			Name:  "qwen-turbo",
			Spec: llm.ModelSpec{
				Caps:   qwenChatCaps,
				Limits: llm.ModelLimits{MaxContextTokens: 1_000_000},
			},
		},
		{
			// Successor SKU to qwen-turbo (per Appaca comparison);
			// shares the 1M context ceiling.
			Label: "Qwen Flash",
			Name:  "qwen-flash",
			Spec: llm.ModelSpec{
				Caps:   qwenChatCaps,
				Limits: llm.ModelLimits{MaxContextTokens: 1_000_000},
			},
		},
	}
	llm.RegisterProviderModels("qwen", qwenModels)
}

const (
	defaultResponsesBaseURL = "https://dashscope.aliyuncs.com/api/v2/apps/protocols/compatible-mode/v1"
	defaultModel            = "qwen-flash"
)

// LLM wraps the OpenAI-compatible Responses adapter to handle Qwen-specific parameters.
type LLM struct {
	inner *openai.LLM
}

// New creates a Qwen LLM instance backed by DashScope's Responses API.
func New(model, apiKey, baseURL string) (*LLM, error) {
	if model == "" {
		model = defaultModel
	}
	inner, err := openai.New(model, apiKey, qwenResponsesBaseURL(baseURL))
	if err != nil {
		return nil, err
	}
	// Tag the OTel/metrics provider as "qwen" so dashboards split out
	// Qwen traffic from the upstream OpenAI-compatible transport. See
	// sdkx/llm/openai/openai.go ▸ WithProviderName for the contract.
	inner.WithProviderName("qwen")
	return &LLM{inner: inner}, nil
}

func (q *LLM) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return q.inner.Generate(ctx, msgs, injectResponsesThinking(opts)...)
}

func (q *LLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	return q.inner.GenerateStream(ctx, msgs, injectResponsesThinking(opts)...)
}

func (q *LLM) Provider() string {
	if q == nil || q.inner == nil {
		return "qwen"
	}
	return q.inner.Provider()
}

func qwenResponsesBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return defaultResponsesBaseURL
	}
	if strings.HasSuffix(baseURL, "/api/v2/apps/protocols/compatible-mode/v1") {
		return baseURL
	}
	if before, ok := strings.CutSuffix(baseURL, "/compatible-mode/v1"); ok {
		return before + "/api/v2/apps/protocols/compatible-mode/v1"
	}
	return baseURL
}

func injectResponsesThinking(opts []llm.GenerateOption) []llm.GenerateOption {
	o := llm.ApplyOptions(opts...)
	if o.Thinking == nil {
		return append(opts, llm.WithExtra("enable_thinking", false))
	}
	return append(opts, llm.WithExtra("enable_thinking", *o.Thinking))
}
