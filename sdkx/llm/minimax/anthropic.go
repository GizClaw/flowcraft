package minimax

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/llm/anthropic"
)

func init() {
	llm.RegisterProvider("minimax", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	// Catalog reflects MiniMax's public lineup as of 2026-07-10. Sources:
	//   - https://platform.minimax.io/docs/api-reference/api-overview
	//   - https://platform.minimax.io/docs/api-reference/text-anthropic-api
	//   - https://www.minimax.io/news/minimax-m25
	//   - https://github.com/MiniMax-AI/MiniMax-M2
	//   - https://developer.nvidia.com/blog/minimax-m2-7-advances-scalable-agentic-workflows-on-nvidia-platforms-for-complex-ai-applications/
	//
	// Notes:
	//   - The M2 series is text-only at the model layer; image/video
	//     features ship as a separate MiniMax MCP product, so
	//     CapVision / CapAudio / CapFile are disabled for M2.x entries.
	//   - MiniMax-M3 is the current flagship: 1M context, multimodal
	//     input (text, image, video), tool use, and thinking. It keeps
	//     CapVision enabled but still disables audio/file input (no
	//     public API support) and all output modalities (MiniMax-M3 is
	//     not an image/audio generator).
	//   - This adapter dials MiniMax's Anthropic-compatible endpoint
	//     (defaultBaseURL = ".../anthropic") through the Anthropic
	//     Go SDK — see New() below, which delegates to
	//     sdkx/llm/anthropic. Both JSON caps are disabled here based
	//     on confirmed-no-support evidence (not just protocol guess):
	//
	//       * CapJSONSchema — same as the upstream anthropic adapter:
	//         GenerateOptions.JSONSchema is not translated and would
	//         be silently dropped.
	//       * CapJSONMode — MiniMax's /anthropic endpoint
	//         documentation lists supported fields as
	//         model/messages/max_tokens/system/tools/tool_choice;
	//         output_format is NOT supported. Their OpenAI-compatible
	//         endpoint also rejects (silently ignores)
	//         response_format on M2-series — see
	//         github.com/MiniMax-AI/MiniMax-M2.5/issues/4.
	//         response_format is only honored on legacy SKUs like
	//         MiniMax-Text-01 (not M2/M2.5/M2.7/M3).
	//
	//     Structured output on MiniMax today is prompt-engineering only
	//     (system-message instructions asking for JSON). The upstream
	//     caps middleware will pass such requests straight through.
	//
	//   - The /anthropic endpoint docs list top_k, stop_sequences,
	//     frequency_penalty, and presence_penalty as ignored; disable
	//     the corresponding caps so callers get a clear signal instead
	//     of a silent no-op. The same docs list thinking as only
	//     accepting {"type":"adaptive"}, which the upstream Anthropic
	//     adapter emits as {"type":"enabled","budget_tokens":N}, so
	//     CapThinking is disabled for the M2.x entries.
	//
	//   - Context window is 200K for M2.x and 1M for M3.

	// Cap set shared by M2.x and M3: parameters the /anthropic
	// endpoint docs explicitly list as ignored, plus unsupported
	// output modalities and JSON caps.
	commonCaps := []llm.Capability{
		llm.CapAudio, llm.CapFile,
		llm.CapJSONMode, llm.CapJSONSchema,
		llm.CapTopK,
		llm.CapStopWords,
		llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		// Output: text only. Image generation lives in the
		// minimax-image adapter (sdkx/llm/minimax/image); audio
		// generation has no in-tree adapter today.
		llm.CapImageOutput, llm.CapAudioOutput,
	}

	// M2.x is text-only and the MiniMax /anthropic docs list thinking
	// as only {"type":"adaptive"}, while the upstream Anthropic SDK
	// emits {"type":"enabled","budget_tokens":N}. Disable thinking and
	// vision for this family.
	textOnlyAnthropicStyle := llm.DisabledCaps(
		append(append([]llm.Capability(nil), commonCaps...), llm.CapVision, llm.CapThinking)...,
	)

	// M3 keeps vision, tools, tool_choice, and thinking enabled; only
	// audio/file input, JSON caps, and ignored sampling parameters are
	// stripped. Output remains text-only.
	multimodalAnthropicStyle := llm.DisabledCaps(commonCaps...)

	llm.RegisterProviderModels("minimax", []llm.ModelInfo{
		// --- M3 generation (current flagship) -----------------------
		// Sources:
		//   - https://platform.minimax.io/docs/api-reference/api-overview
		//   - https://platform.minimax.io/docs/api-reference/text-anthropic-api
		//
		// 1M context, multimodal input (text/image/video), tool use,
		// thinking, and interleaved thinking. Listed in the Anthropic-
		// compatible endpoint, so this adapter can drive it directly.
		{
			Label: "MiniMax-M3",
			Name:  "MiniMax-M3",
			Spec: llm.ModelSpec{
				Caps:   multimodalAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 1_000_000},
			},
		},

		// --- M2.7 generation (previous flagship) --------------------
		// Sources:
		//   - https://platform.minimax.io/docs/api-reference/api-overview
		//   - https://platform.minimax.io/docs/api-reference/text-anthropic-api
		//   - https://artificialanalysis.ai/articles/minimax-m2-7-everything-you-need-to-know
		//   - https://blog.galaxy.ai/model/minimax-m2-7
		//
		// Context window 204,800 tokens; max output ~131,072 tokens
		// per Galaxy.ai snapshot. M2.7 is reasoning-only, text-only.
		{
			Label: "MiniMax-M2.7",
			Name:  "MiniMax-M2.7",
			Spec: llm.ModelSpec{
				Caps: textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{
					MaxContextTokens: 204_800,
					MaxOutputTokens:  131_072,
				},
			},
		},
		{
			Label: "MiniMax-M2.7 HighSpeed",
			Name:  "MiniMax-M2.7-highspeed",
			Spec: llm.ModelSpec{
				Caps: textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{
					MaxContextTokens: 204_800,
					MaxOutputTokens:  131_072,
				},
			},
		},

		// --- M2.5 generation ----------------------------------------
		{
			Label: "MiniMax-M2.5",
			Name:  "MiniMax-M2.5",
			Spec: llm.ModelSpec{
				Caps:   textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.5 HighSpeed",
			Name:  "MiniMax-M2.5-highspeed",
			Spec: llm.ModelSpec{
				Caps:   textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},

		// --- M2.1 (intermediate cost-efficient) --------------------
		// Listed in the Anthropic-compat endpoint per
		// platform.minimax.io/docs/api-reference/text-anthropic-api.
		{
			Label: "MiniMax-M2.1",
			Name:  "MiniMax-M2.1",
			Spec: llm.ModelSpec{
				Caps:   textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.1 HighSpeed",
			Name:  "MiniMax-M2.1-highspeed",
			Spec: llm.ModelSpec{
				Caps:   textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},

		// --- M2 (legacy base; not in Anthropic-compat list) --------
		// Note: As of 2026-04-30 the docs page
		// platform.minimax.io/docs/api-reference/text-anthropic-api
		// no longer lists "MiniMax-M2" among the Anthropic-compatible
		// SKUs (only M2.7 / M2.7-highspeed / M2.5 / M2.5-highspeed /
		// M2.1). Kept here for backward compatibility — calls may
		// fail at the endpoint. Prefer M2.1 or higher for new code.
		{
			Label: "MiniMax-M2",
			Name:  "MiniMax-M2",
			Spec: llm.ModelSpec{
				Caps:   textOnlyAnthropicStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
	})
}

const (
	defaultBaseURL = "https://api.minimaxi.com/anthropic"
	defaultModel   = "MiniMax-M2.5"
)

// New creates a MiniMax LLM instance (Anthropic-compatible).
func New(model, apiKey, baseURL string) (*anthropic.LLM, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	inner, err := anthropic.New(model, apiKey, baseURL, nil)
	if err != nil {
		return nil, err
	}
	// Tag the OTel/metrics provider as "minimax" so MiniMax traffic
	// (which speaks the Anthropic Messages API over /anthropic) is
	// observable as its own bucket instead of being aggregated under
	// the upstream "anthropic" tag. See sdkx/llm/anthropic/anthropic.go
	// ▸ WithProviderName.
	return inner.WithProviderName("minimax"), nil
}
