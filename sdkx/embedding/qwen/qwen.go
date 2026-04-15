package qwen

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	embeddingOpenAI "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	"github.com/openai/openai-go/option"
)

const (
	defaultBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	defaultModel   = "text-embedding-v4"
)

func init() {
	embedding.RegisterProvider("qwen", func(model string, config map[string]any) (embedding.Embedder, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(apiKey, model, baseURL)
	})
}

// New creates a Qwen/DashScope embedder. It reuses the OpenAI embedder
// with DashScope's OpenAI-compatible endpoint.
func New(apiKey, model, baseURL string) (*embeddingOpenAI.Embedder, error) {
	if apiKey == "" {
		return nil, embedding.ErrMissingCredentials
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	e := embeddingOpenAI.New(apiKey, model, option.WithBaseURL(baseURL))
	if e == nil {
		return nil, embedding.ErrMissingCredentials
	}
	return e, nil
}
