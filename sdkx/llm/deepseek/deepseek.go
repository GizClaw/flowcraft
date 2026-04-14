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

	llm.RegisterProviderModels("deepseek", []llm.ModelInfo{
		{Label: "DeepSeek Chat", Name: "deepseek-chat", Caps: llm.DisabledCaps(llm.CapJSONSchema)},
		{Label: "DeepSeek Reasoner", Name: "deepseek-reasoner", Caps: llm.DisabledCaps(llm.CapTemperature, llm.CapJSONSchema)},
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
