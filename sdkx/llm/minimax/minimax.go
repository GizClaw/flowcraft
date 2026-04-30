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

	// Catalog reflects MiniMax's M2-series public lineup as of
	// 2026-04-30. Sources:
	//   - https://www.minimax.io/news/minimax-m25
	//   - https://github.com/MiniMax-AI/MiniMax-M2
	//   - https://developer.nvidia.com/blog/minimax-m2-7-advances-scalable-agentic-workflows-on-nvidia-platforms-for-complex-ai-applications/
	//
	// Notes:
	//   - The M2 series is text-only at the model layer; image/video
	//     features ship as a separate MiniMax MCP product, so
	//     CapVision / CapAudio / CapFile are disabled here to
	//     fail-fast at the caps middleware.
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
	//         MiniMax-Text-01 (not M2/M2.5/M2.7).
	//
	//     Structured output on MiniMax M2-series today is
	//     prompt-engineering only (system-message instructions
	//     asking for JSON). The upstream caps middleware will pass
	//     such requests straight through.
	//
	//   - Context window is 200K per the MiniMax-M2 model card.
	textOnlyAnthropicStyle := llm.DisabledCaps(
		llm.CapVision, llm.CapAudio, llm.CapFile,
		llm.CapJSONMode, llm.CapJSONSchema,
	)

	llm.RegisterProviderModels("minimax", []llm.ModelInfo{
		// --- M2.7 generation (current default flagship) -------------
		// Sources:
		//   - https://platform.minimax.io/docs/api-reference/api-overview
		//   - https://platform.minimax.io/docs/api-reference/text-anthropic-api
		//   - https://artificialanalysis.ai/articles/minimax-m2-7-everything-you-need-to-know
		//   - https://blog.galaxy.ai/model/minimax-m2-7
		//
		// Context window 204,800 tokens; max output ~131,072 tokens
		// per Galaxy.ai snapshot. M2.7 is reasoning-only, text-only
		// per artificialanalysis.ai. Listed in the Anthropic-compat
		// API endpoint, so this adapter (which dials /anthropic) can
		// drive it directly.
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
	return anthropic.New(model, apiKey, baseURL, nil)
}
