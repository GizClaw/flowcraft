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
	qwenChatCaps := llm.DisabledCaps(llm.CapAudio, llm.CapFile)

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

// LLM wraps openai.LLM to handle Qwen-specific parameters.
type LLM struct {
	inner *openai.LLM
}

// New creates a Qwen LLM instance. Wraps openai.LLM to inject
// enable_thinking based on GenerateOptions.Thinking.
func New(model, apiKey, baseURL string) (*LLM, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	inner, err := openai.New(model, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	return &LLM{inner: inner}, nil
}

func (q *LLM) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return q.inner.Generate(ctx, msgs, append(opts, injectThinking(opts))...)
}

func (q *LLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	return q.inner.GenerateStream(ctx, msgs, append(opts, injectThinking(opts))...)
}

// injectThinking maps GenerateOptions.Thinking to Qwen's enable_thinking
// body field via Extra. When Thinking is nil, defaults to false (Qwen3
// commercial models have thinking disabled by default, but some need
// the field explicitly).
func injectThinking(opts []llm.GenerateOption) llm.GenerateOption {
	o := llm.ApplyOptions(opts...)
	enable := o.Thinking != nil && *o.Thinking
	return llm.WithExtra("enable_thinking", enable)
}
