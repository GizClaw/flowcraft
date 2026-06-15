package openai

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/llm/openai/chat"
	"github.com/GizClaw/flowcraft/sdkx/llm/openai/responses"

	"github.com/openai/openai-go/option"
)

func init() {
	llm.RegisterProvider("openai", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})
	llm.RegisterProvider("openai-chat", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return NewChat(model, apiKey, baseURL)
	})

	// Catalog reflects OpenAI's public model lineup as of 2026-04-30.
	// Sources:
	//   - https://developers.openai.com/api/docs/models/gpt-5.4
	//   - https://developers.openai.com/api/docs/models/gpt-5.4-pro
	//   - https://openai.com/index/introducing-gpt-5-4/
	//   - https://openai.com/index/introducing-gpt-5-4-mini-and-nano/
	//   - https://openai.com/gpt-5/
	//
	// The default "openai" provider is the Responses API catalog.
	// The "openai-chat" catalog is derived below and omits models that
	// are Responses-only. Across the shared gpt-5 / gpt-5.4 entries,
	// text+image input is the portable surface; audio and file
	// modalities go through separate APIs (gpt-4o-audio-preview /
	// Files endpoint), so CapAudio and CapFile are disabled here to
	// fail-fast at the caps middleware rather than at the OpenAI API edge.
	//
	// Output modality: text only. Image generation goes through the
	// dedicated gpt-image-1 / Images API surface (separate adapter),
	// and audio output goes through gpt-realtime-* / TTS endpoints.
	// Verified against developers.openai.com/api/docs/models/gpt-5
	// and developers.openai.com/docs/guides/audio (Chat Completions
	// audio path covers gpt-realtime-* SKUs only) as of 2026-04-30.
	openaiTextImageOnly := llm.DisabledCaps(
		llm.CapAudio, llm.CapFile,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	responsesModels := []llm.ModelInfo{
		// --- GPT-5.5 (current flagship, released 2026-04-23) -------
		// Sources:
		//   - https://openai.com/index/introducing-gpt-5-5/
		//   - https://developers.openai.com/api/docs/models/gpt-5.5
		//   - https://llm-stats.com/blog/research/gpt-5-5-vs-gpt-5-4
		//
		// 1.05M ctx / 128K output. Text + image input. Tool use,
		// streaming, function calling all supported on the standard
		// Chat Completions / Responses APIs.
		{
			Label: "GPT-5.5",
			Name:  "gpt-5.5",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_050_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			Label: "GPT-5.5 Pro",
			Name:  "gpt-5.5-pro",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_050_000,
					MaxOutputTokens:  128_000,
				},
			},
		},

		{
			Label: "GPT-5.4",
			Name:  "gpt-5.4",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_050_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			Label: "GPT-5.4 Pro",
			Name:  "gpt-5.4-pro",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 1_050_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			// GPT-5 base family share 400K context / 128K output per
			// openai.com/gpt-5/.
			Label: "GPT-5",
			Name:  "gpt-5",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 400_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			Label: "GPT-5 nano",
			Name:  "gpt-5-nano",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 400_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			Label: "GPT-5 Mini",
			Name:  "gpt-5-mini",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 400_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		// gpt-4.1 was removed from this catalog on 2026-04-30. It is
		// still served by the OpenAI API at the time of writing, but
		// is positioned as a migration target away from for new work
		// (Azure Foundry Standard already retired 2026-03-31; direct
		// API has no published shutdown date but official guidance
		// recommends gpt-5+). Callers that pinned the name still go
		// through r.registry.NewFromConfig — they will hit the
		// provider directly without spec wrapping. Re-add here with
		// ModelInfo.Deprecation set if a graceful warn-and-serve
		// window is needed; see ModelDeprecation in sdk/llm.

		// --- GPT-5.4 mini / nano (subagent-tier) --------------------
		// Sources:
		//   - https://openai.com/index/introducing-gpt-5-4-mini-and-nano/
		//   - https://developers.openai.com/api/docs/models
		//   - https://apxml.com/models/gpt-54-nano
		{
			Label: "GPT-5.4 Mini",
			Name:  "gpt-5.4-mini",
			Spec: llm.ModelSpec{
				Caps: openaiTextImageOnly,
				Limits: llm.ModelLimits{
					MaxContextTokens: 400_000,
					MaxOutputTokens:  128_000,
				},
			},
		},
		{
			// Smallest cost-efficient variant. Per apxml.com/models/gpt-54-nano
			// the surface is text-only (no image input). Limits not
			// stated authoritatively in the 2026 snapshot; left unset.
			Label: "GPT-5.4 Nano",
			Name:  "gpt-5.4-nano",
			Spec: llm.ModelSpec{
				Caps: llm.DisabledCaps(
					llm.CapVision, llm.CapAudio, llm.CapFile,
					llm.CapImageOutput, llm.CapAudioOutput,
				),
			},
		},

		// --- o-series reasoning models ------------------------------
		// Sources:
		//   - https://developers.openai.com/api/docs/models/o3-pro
		//   - https://openai.com/index/introducing-o3-and-o4-mini/
		//
		// o-series convention (carried forward from o1): sampling
		// controls (temperature, top_p, presence/frequency penalties)
		// are unavailable, and Chat Completions also maps system
		// instructions to developer messages. Caps below reflect the
		// portable restrictions; verify against the current model card
		// if behavior changes.
		{
			Label: "o3",
			Name:  "o3",
			Spec: llm.ModelSpec{
				Caps: llm.DisabledCaps(
					llm.CapTemperature, llm.CapTopP, llm.CapTopK,
					llm.CapFrequencyPenalty, llm.CapPresencePenalty,
					llm.CapStopWords,
					llm.CapAudio, llm.CapFile,
					llm.CapImageOutput, llm.CapAudioOutput,
				),
				Limits: llm.ModelLimits{MaxContextTokens: 200_000},
			},
		},
		{
			// Responses-only. Keep this out of the "openai-chat" catalog.
			Label: "o3 Pro",
			Name:  "o3-pro",
			Spec: llm.ModelSpec{
				Caps: llm.DisabledCaps(
					llm.CapTemperature, llm.CapTopP, llm.CapTopK,
					llm.CapFrequencyPenalty, llm.CapPresencePenalty,
					llm.CapStopWords,
					llm.CapAudio, llm.CapFile,
					llm.CapImageOutput, llm.CapAudioOutput,
				),
				Limits: llm.ModelLimits{
					MaxContextTokens: 200_000,
					MaxOutputTokens:  100_000,
				},
			},
		},
		{
			// DEPRECATED. Per the 2026-04-22 deprecation announcement
			// at developers.openai.com/api/docs/deprecations, the
			// alias `o4-mini` (pointing at snapshot
			// `o4-mini-2025-04-16`) is scheduled for shutdown on
			// 2026-10-23 with `gpt-5-mini` as the recommended
			// replacement. Resolver emits a one-shot telemetry
			// warning per process when this model is resolved —
			// see ModelDeprecation in sdk/llm.
			Label: "o4 Mini",
			Name:  "o4-mini",
			Spec: llm.ModelSpec{
				Caps: llm.DisabledCaps(
					llm.CapTemperature, llm.CapTopP, llm.CapTopK,
					llm.CapFrequencyPenalty, llm.CapPresencePenalty,
					llm.CapStopWords,
					llm.CapAudio, llm.CapFile,
					llm.CapImageOutput, llm.CapAudioOutput,
				),
			},
			Deprecation: llm.ModelDeprecation{
				RetiresAt:   time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
				Replacement: "openai/gpt-5-mini",
				Notes:       "https://developers.openai.com/api/docs/deprecations (2026-04-22 batch)",
			},
		},
	}

	llm.RegisterProviderModels("openai", responsesModels)
	llm.RegisterProviderModels("openai-chat", withoutModelNames(responsesModels, "o3-pro"))
}

func withoutModelNames(models []llm.ModelInfo, names ...string) []llm.ModelInfo {
	excluded := make(map[string]bool, len(names))
	for _, name := range names {
		excluded[name] = true
	}

	filtered := make([]llm.ModelInfo, 0, len(models))
	for _, model := range models {
		if excluded[model.Name] {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

// LLM is the default OpenAI adapter type backed by the Responses API.
type LLM = responses.LLM

// ChatLLM is the historical Chat Completions adapter type. OpenAI-compatible
// providers that do not support Responses API should depend on this explicitly.
type ChatLLM = chat.LLM

// NewChat creates the historical OpenAI Chat Completions adapter.
func NewChat(model, apiKey, baseURL string, opts ...option.RequestOption) (*ChatLLM, error) {
	return chat.New(model, apiKey, baseURL, opts...)
}

// New creates an OpenAI Responses API adapter.
func New(model, apiKey, baseURL string, opts ...option.RequestOption) (*LLM, error) {
	return responses.New(model, apiKey, baseURL, opts...)
}
