package deepseek

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

func init() {
	llm.RegisterProvider("deepseek", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects DeepSeek's public API as of 2026-04-30.
	// Sources:
	//   - https://api-docs.deepseek.com/quick_start/pricing
	//   - https://api-docs.deepseek.com/news/news260424   (V4 Preview)
	//   - https://api-docs.deepseek.com/guides/reasoning_model
	//   - https://api-docs.deepseek.com/guides/json_mode
	//
	// DEPRECATION NOTICE: per news/news260424 the legacy aliases
	// `deepseek-chat` and `deepseek-reasoner` are routed to
	// `deepseek-v4-flash` (non-thinking and thinking modes
	// respectively) and scheduled for retirement on 2026-07-24.
	// Callers should migrate to `deepseek-v4-flash` /
	// `deepseek-v4-pro`. The two aliases are kept here so existing
	// deployments keep resolving until the cutover.
	//
	// CapJSONSchema is disabled across the family: the DeepSeek API
	// only supports `response_format: {"type": "json_object"}`
	// (free-form JSON), not schema-constrained structured output.
	// CapVision / CapAudio / CapFile are disabled because the V4
	// series is text-only per artificialanalysis.ai/articles/...-v4-pro-and-v4-flash.
	deepseekTextOnly := llm.DisabledCaps(
		llm.CapJSONSchema, llm.CapVision, llm.CapAudio, llm.CapFile,
	)

	llm.RegisterProviderModels("deepseek", []llm.ModelInfo{
		// --- V4 series (current) -------------------------------------
		{
			// Released 2026-04-24 in V4 Preview. 284B MoE / 13B active.
			// Hybrid thinking + non-thinking; tools and JSON output
			// supported.
			Label: "DeepSeek V4 Flash",
			Name:  "deepseek-v4-flash",
			Spec: llm.ModelSpec{
				Caps: deepseekTextOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  384_000,
				},
			},
		},
		{
			// Flagship reasoning model. 1.6T MoE / 49B active per
			// huggingface.co/deepseek-ai/DeepSeek-V4-Pro and
			// together.ai/models/deepseek-v4-pro. Same 1M/384K limits
			// as Flash, hybrid thinking + non-thinking, tools + JSON.
			Label: "DeepSeek V4 Pro",
			Name:  "deepseek-v4-pro",
			Spec: llm.ModelSpec{
				Caps: deepseekTextOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  384_000,
				},
			},
		},

		// --- Legacy aliases (retire 2026-07-24) ---------------------
		// Per news/news260424 the two names below are routed to
		// deepseek-v4-flash (non-thinking and thinking modes
		// respectively) and scheduled for retirement on 2026-07-24
		// 15:59 UTC. Kept here so existing deployments keep
		// resolving until the cutover; new code SHOULD pin
		// deepseek-v4-flash / deepseek-v4-pro directly.
		{
			// Routes to deepseek-v4-flash non-thinking. Legacy
			// max-output cap of 8K is preserved on the alias even
			// after routing, per news/news260424.
			Label: "DeepSeek Chat (legacy alias → v4-flash)",
			Name:  "deepseek-chat",
			Spec: llm.ModelSpec{
				Caps: deepseekTextOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  8_000,
				},
			},
		},
		{
			// Routes to deepseek-v4-flash thinking mode. Per
			// guides/reasoning_model the thinking surface does NOT
			// support function calling, sampling controls
			// (temperature/top_p), or penalty knobs. 64K max output.
			Label: "DeepSeek Reasoner (legacy alias → v4-flash thinking)",
			Name:  "deepseek-reasoner",
			Spec: llm.ModelSpec{
				Caps: llm.DisabledCaps(
					llm.CapTemperature,
					llm.CapTopP,
					llm.CapFrequencyPenalty,
					llm.CapPresencePenalty,
					llm.CapTools,
					llm.CapToolChoice,
					llm.CapParallelTools,
					llm.CapJSONSchema,
					llm.CapVision,
					llm.CapAudio,
					llm.CapFile,
				),
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_000_000,
					MaxOutputTokens:  64_000,
				},
			},
		},
	})
}

const (
	defaultBaseURL = "https://api.deepseek.com/v1"
	defaultModel   = "deepseek-chat"
)

// New creates a DeepSeek LLM instance (OpenAI-compatible).
func New(model, apiKey, baseURL string) (*openai.LLM, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	return openai.New(model, apiKey, baseURL)
}
