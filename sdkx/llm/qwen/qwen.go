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

	llm.RegisterProviderModels("qwen", []llm.ModelInfo{
		{Label: "Qwen Max", Name: "qwen-max"},
		{Label: "Qwen Plus", Name: "qwen-plus"},
		{Label: "Qwen Turbo", Name: "qwen-turbo"},
		{Label: "Qwen Flash", Name: "qwen-flash"},
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
