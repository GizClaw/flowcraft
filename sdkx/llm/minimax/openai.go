// Package minimax registers MiniMax's Anthropic-compatible and
// OpenAI-compatible LLM providers.
package minimax

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	openaiadapter "github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

func init() {
	llm.RegisterProvider("minimax-oai", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return NewOpenAI(model, apiKey, baseURL)
	})

	// MiniMax's OpenAI-compatible endpoint works with openai-go for chat
	// completions. Do not advertise JSON mode or default reasoning_split
	// here: M2.x response_format support is unreliable, and
	// reasoning_split requires preserving reasoning_details in history.
	textOnlyOpenAIStyle := llm.DisabledCaps(
		llm.CapVision, llm.CapAudio, llm.CapFile,
		llm.CapJSONSchema, llm.CapJSONMode,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	llm.RegisterProviderModels("minimax-oai", []llm.ModelInfo{
		{
			Label: "MiniMax-M2.7",
			Name:  "MiniMax-M2.7",
			Spec: llm.ModelSpec{
				Caps: textOnlyOpenAIStyle,
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
				Caps: textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{
					MaxContextTokens: 204_800,
					MaxOutputTokens:  131_072,
				},
			},
		},
		{
			Label: "MiniMax-M2.5",
			Name:  "MiniMax-M2.5",
			Spec: llm.ModelSpec{
				Caps:   textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.5 HighSpeed",
			Name:  "MiniMax-M2.5-highspeed",
			Spec: llm.ModelSpec{
				Caps:   textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.1",
			Name:  "MiniMax-M2.1",
			Spec: llm.ModelSpec{
				Caps:   textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.1 HighSpeed",
			Name:  "MiniMax-M2.1-highspeed",
			Spec: llm.ModelSpec{
				Caps:   textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2",
			Name:  "MiniMax-M2",
			Spec: llm.ModelSpec{
				Caps:   textOnlyOpenAIStyle,
				Limits: llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
	})
}

const defaultOpenAIBaseURL = "https://api.minimax.io/v1"

// NewOpenAI creates a MiniMax LLM instance through MiniMax's
// OpenAI-compatible chat completions endpoint.
func NewOpenAI(model, apiKey, baseURL string) (*openaiadapter.LLM, error) {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	inner, err := openaiadapter.New(model, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	return inner.WithProviderName("minimax-oai"), nil
}
