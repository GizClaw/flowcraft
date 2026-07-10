package qwen

import (
	"context"

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
		llm.CapJSONSchema,
		llm.CapFrequencyPenalty,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	llm.RegisterProviderModels("qwen", []llm.ModelInfo{
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
	})
}

const (
	defaultBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	defaultModel   = "qwen-flash"
)

// LLM wraps openai.ChatLLM to handle Qwen-specific parameters.
type LLM struct {
	inner *openai.ChatLLM
}

// New creates a Qwen LLM instance. Wraps the Chat Completions adapter
// to inject enable_thinking based on GenerateOptions.Thinking.
func New(model, apiKey, baseURL string) (*LLM, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	inner, err := openai.NewChat(model, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	// Tag the OTel/metrics provider as "qwen" so dashboards split out
	// Qwen traffic from the upstream openai.ChatLLM that delegates the HTTP
	// transport. See sdkx/llm/openai/openai.go ▸ WithProviderName for
	// the contract.
	inner.WithProviderName("qwen")
	return &LLM{inner: inner}, nil
}

func (q *LLM) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return q.inner.Generate(ctx, msgs, append(opts, qwenExtras(opts)...)...)
}

func (q *LLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	return q.inner.GenerateStream(ctx, msgs, append(opts, qwenExtras(opts)...)...)
}

// qwenExtras maps Qwen-specific GenerateOptions fields to Extra body keys
// that the OpenAI-compatible baseline does not emit. The baseline sends
// max_completion_tokens, which Qwen's compatible endpoint silently ignores;
// Qwen still honors the legacy max_tokens field, so we mirror MaxTokens
// there. top_k is a Qwen-specific sampling parameter the baseline never
// sets. enable_thinking is Qwen's thinking toggle.
func qwenExtras(opts []llm.GenerateOption) []llm.GenerateOption {
	o := llm.ApplyOptions(opts...)
	out := []llm.GenerateOption{
		llm.WithExtra("enable_thinking", o.Thinking != nil && *o.Thinking),
	}
	if o.MaxTokens != nil {
		out = append(out, llm.WithExtra("max_tokens", *o.MaxTokens))
	}
	if o.TopK != nil {
		out = append(out, llm.WithExtra("top_k", *o.TopK))
	}
	return out
}
