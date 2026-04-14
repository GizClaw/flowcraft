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

	llm.RegisterProviderModels("minimax", []llm.ModelInfo{
		{Label: "MiniMax-M2.5", Name: "MiniMax-M2.5"},
		{Label: "MiniMax-M2.5 HighSpeed", Name: "MiniMax-M2.5-highspeed"},
		{Label: "MiniMax-M2", Name: "MiniMax-M2"},
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
