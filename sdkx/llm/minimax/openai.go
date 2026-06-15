// Package minimax registers MiniMax's Anthropic-compatible and
// OpenAI-compatible LLM providers.
package minimax

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	openaiadapter "github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

func init() {
	llm.RegisterProvider("minimax-oai", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return NewOpenAI(model, apiKey, baseURL)
	})

	// MiniMax's OpenAI-compatible endpoint uses Responses API semantics.
	// Do not advertise JSON mode or default reasoning_split here:
	// Responses text.format only documents text, and reasoning_split is
	// a Chat-only parameter.
	textOnlyOpenAIStyle := llm.DisabledCaps(
		llm.CapVision, llm.CapAudio, llm.CapFile,
		llm.CapJSONSchema, llm.CapJSONMode,
		llm.CapImageOutput, llm.CapAudioOutput,
	)

	llm.RegisterProviderModels("minimax-oai", []llm.ModelInfo{
		{
			Label: "MiniMax-M3",
			Name:  "MiniMax-M3",
			Spec: llm.ModelSpec{
				Caps: textOnlyOpenAIStyle,
			},
		},
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

const (
	defaultOpenAIBaseURL = "https://api.minimax.io/v1"
	defaultOpenAIModel   = "MiniMax-M3"
)

// OpenAILLM wraps MiniMax's OpenAI-compatible Responses API.
type OpenAILLM struct {
	inner *openaiadapter.LLM
}

// NewOpenAI creates a MiniMax LLM instance through MiniMax's
// OpenAI-compatible Responses endpoint.
func NewOpenAI(model, apiKey, baseURL string) (*OpenAILLM, error) {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if model == "" {
		model = defaultOpenAIModel
	}
	inner, err := openaiadapter.New(model, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	inner.WithProviderName("minimax-oai")
	return &OpenAILLM{inner: inner}, nil
}

func (m *OpenAILLM) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	responsesOpts, err := miniMaxResponsesOptions(opts)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	return m.inner.Generate(ctx, msgs, responsesOpts...)
}

func (m *OpenAILLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	responsesOpts, err := miniMaxResponsesOptions(opts)
	if err != nil {
		return nil, err
	}
	return m.inner.GenerateStream(ctx, msgs, responsesOpts...)
}

func (m *OpenAILLM) Provider() string {
	if m == nil || m.inner == nil {
		return "minimax-oai"
	}
	return m.inner.Provider()
}

func miniMaxResponsesOptions(opts []llm.GenerateOption) ([]llm.GenerateOption, error) {
	if err := validateMiniMaxResponsesOptions(llm.ApplyOptions(opts...)); err != nil {
		return nil, err
	}
	return injectMiniMaxResponsesThinking(opts), nil
}

func validateMiniMaxResponsesOptions(opts *llm.GenerateOptions) error {
	if opts.JSONSchema != nil {
		return errdefs.Validation(fmt.Errorf("minimax-oai responses: JSON schema output is not supported"))
	}
	if opts.JSONMode != nil && *opts.JSONMode {
		return errdefs.Validation(fmt.Errorf("minimax-oai responses: JSON mode is not supported"))
	}
	if opts.ToolChoice == nil {
		return nil
	}
	switch opts.ToolChoice.Type {
	case llm.ToolChoiceNone, llm.ToolChoiceAuto:
		return nil
	default:
		return errdefs.Validation(fmt.Errorf("minimax-oai responses: tool_choice %q is not supported; use none or auto", opts.ToolChoice.Type))
	}
}

func injectMiniMaxResponsesThinking(opts []llm.GenerateOption) []llm.GenerateOption {
	o := llm.ApplyOptions(opts...)
	if o.Thinking == nil {
		return opts
	}
	effort := "none"
	if *o.Thinking {
		effort = "minimal"
	}
	return append(opts, llm.WithExtra("reasoning.effort", effort))
}
