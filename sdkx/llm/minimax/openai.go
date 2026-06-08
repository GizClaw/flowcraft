package minimax

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	openaiadapter "github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

func init() {
	llm.RegisterProvider("minimax-extract", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return NewOpenAI(model, apiKey, baseURL)
	})

	// MiniMax's OpenAI-compatible endpoint works with openai-go for chat
	// completions. M2-series supports response_format=json_object when
	// reasoning_split=true, but not strict JSON schema reliably; let the
	// caps wrapper downgrade schema requests to JSON mode.
	textOnlyOpenAIStyle := llm.DisabledCaps(
		llm.CapVision, llm.CapAudio, llm.CapFile,
		llm.CapJSONSchema,
		llm.CapImageOutput, llm.CapAudioOutput,
	)
	openAIStyleDefaults := llm.GenerateOptions{
		Extra: map[string]any{"reasoning_split": true},
	}

	llm.RegisterProviderModels("minimax-extract", []llm.ModelInfo{
		{
			Label: "MiniMax-M2.7",
			Name:  "MiniMax-M2.7",
			Spec: llm.ModelSpec{
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
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
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
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
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
				Limits:   llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.5 HighSpeed",
			Name:  "MiniMax-M2.5-highspeed",
			Spec: llm.ModelSpec{
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
				Limits:   llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.1",
			Name:  "MiniMax-M2.1",
			Spec: llm.ModelSpec{
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
				Limits:   llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2.1 HighSpeed",
			Name:  "MiniMax-M2.1-highspeed",
			Spec: llm.ModelSpec{
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
				Limits:   llm.ModelLimits{MaxContextTokens: 204_800},
			},
		},
		{
			Label: "MiniMax-M2",
			Name:  "MiniMax-M2",
			Spec: llm.ModelSpec{
				Caps:     textOnlyOpenAIStyle,
				Defaults: openAIStyleDefaults,
				Limits:   llm.ModelLimits{MaxContextTokens: 204_800},
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
	return inner.WithProviderName("minimax-extract"), nil
}
