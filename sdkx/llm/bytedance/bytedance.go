// Package bytedance provides the ByteDance Doubao LLM provider using
// the Volcengine ArkRuntime Go SDK.
package bytedance

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultModel     = "doubao-seed-1-8-251228"
	defaultMaxTokens = 4096
)

func init() {
	llm.RegisterProvider("bytedance", func(modelName string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		region, _ := config["region"].(string)
		retryTimes := 0
		if v, ok := config["retry_times"].(float64); ok {
			retryTimes = int(v)
		}
		return New(modelName, apiKey, baseURL, region, retryTimes)
	})

	// Catalog reflects the Doubao 2.0 launch (2026-02-14) lineup as
	// of 2026-04-30. Sources:
	//   - https://baike.baidu.com/en/item/Doubao-Seed-2.0/1515788
	//   - https://www.binance.com/en/square/post/291464529173265
	//   - https://xairouter.com/en/models/doubao-seed-2.0-mini/
	//   - https://www.reuters.com/world/asia-pacific/chinas-bytedance-releases-doubao-20-ai-chatbot-2026-02-14/
	//
	// All Doubao Seed 2.0 variants share a 256K context window and a
	// 32K max output cap per the unified family doc. Vision and tool
	// use are first-class. The Volcengine Ark API is OpenAI-style and
	// supports streaming + structured output natively, so no
	// negative caps need declaring there.
	llm.RegisterProviderModels("bytedance", []llm.ModelInfo{
		{
			Label: "Doubao Seed 2.0 Pro",
			Name:  "doubao-seed-2-0-pro-260215",
			Spec: llm.ModelSpec{
				Limits: llm.ModelLimits{
					MaxContextTokens: 256_000,
					MaxOutputTokens:  32_000,
				},
			},
		},
		{
			Label: "Doubao Seed 2.0 Lite",
			Name:  "doubao-seed-2-0-lite-260215",
			Spec: llm.ModelSpec{
				Limits: llm.ModelLimits{
					MaxContextTokens: 256_000,
					MaxOutputTokens:  32_000,
				},
			},
		},
		{
			Label: "Doubao Seed 2.0 Mini",
			Name:  "doubao-seed-2-0-mini-260215",
			Spec: llm.ModelSpec{
				Limits: llm.ModelLimits{
					MaxContextTokens: 256_000,
					MaxOutputTokens:  32_000,
				},
			},
		},
		{
			// Predecessor generation, kept for callers pinning the
			// 1.8 SKU. Limits not republished in the 2.0 launch
			// material we have on file; left unset.
			Label: "Doubao Seed 1.8",
			Name:  "doubao-seed-1-8-251228",
		},
	})
}

// LLM implements llm.LLM using the Volcengine ArkRuntime Go SDK.
type LLM struct {
	client *arkruntime.Client
	model  string
}

var _ llm.LLM = (*LLM)(nil)

// New creates a ByteDance LLM instance.
func New(modelName, apiKey, baseURL, region string, retryTimes int) (*LLM, error) {
	if modelName == "" {
		modelName = defaultModel
	}

	var ropts []arkruntime.ConfigOption
	if region != "" {
		ropts = append(ropts, arkruntime.WithRegion(region))
	}
	if baseURL != "" {
		ropts = append(ropts, arkruntime.WithBaseUrl(baseURL))
	}
	if retryTimes > 0 {
		ropts = append(ropts, arkruntime.WithRetryTimes(retryTimes))
	}
	ropts = append(ropts, arkruntime.WithHTTPClient(http.DefaultClient))

	client := arkruntime.NewClientWithApiKey(apiKey, ropts...)
	return &LLM{
		client: client,
		model:  modelName,
	}, nil
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.bytedance.generate.%s", c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, "bytedance"),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	req := c.buildRequest(messages, options)

	start := time.Now()
	resp, err := c.client.CreateChatCompletion(ctx, req)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("bytedance.generate: %s", dur.String())
		}
		return llm.Message{}, llm.TokenUsage{}, errdefs.ClassifyProviderError("bytedance", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0] == nil {
		err := errdefs.NotAvailablef("bytedance: empty choices")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	usage := llm.TokenUsage{
		InputTokens:  int64(resp.Usage.PromptTokens),
		OutputTokens: int64(resp.Usage.CompletionTokens),
		TotalTokens:  int64(resp.Usage.TotalTokens),
	}

	span.SetAttributes(
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens),
	)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "bytedance", c.model, "success", dur, usage)

	msg := convertResponse(resp)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.bytedance.stream.%s", c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, "bytedance"),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))

	options := llm.ApplyOptions(opts...)
	req := c.buildRequest(messages, options)
	req.StreamOptions = &model.StreamOptions{IncludeUsage: true}

	stream, err := c.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", 0, llm.TokenUsage{})
		return nil, errdefs.ClassifyProviderError("bytedance", err)
	}

	return newStreamMessage(ctx, span, c.model, stream), nil
}

func (c *LLM) buildRequest(msgs []llm.Message, opts *llm.GenerateOptions) model.CreateChatCompletionRequest {
	maxTokens := defaultMaxTokens
	if opts.MaxTokens != nil {
		maxTokens = int(*opts.MaxTokens)
	}

	thinkType := model.ThinkingTypeDisabled
	if opts.Thinking != nil && *opts.Thinking {
		thinkType = model.ThinkingTypeEnabled
	}

	req := model.CreateChatCompletionRequest{
		Model:     c.model,
		Messages:  convertMessages(msgs),
		MaxTokens: &maxTokens,
		Thinking:  &model.Thinking{Type: thinkType},
	}

	if opts.Temperature != nil {
		t := float32(*opts.Temperature)
		req.Temperature = &t
	}
	if opts.TopP != nil {
		p := float32(*opts.TopP)
		req.TopP = &p
	}
	if len(opts.StopWords) > 0 {
		req.Stop = opts.StopWords
	}
	if opts.FrequencyPenalty != nil {
		fp := float32(*opts.FrequencyPenalty)
		req.FrequencyPenalty = &fp
	}
	if opts.PresencePenalty != nil {
		pp := float32(*opts.PresencePenalty)
		req.PresencePenalty = &pp
	}
	if opts.JSONMode != nil && *opts.JSONMode {
		req.ResponseFormat = &model.ResponseFormat{
			Type: model.ResponseFormatJsonObject,
		}
	}
	if len(opts.Tools) > 0 {
		tools := make([]*model.Tool, 0, len(opts.Tools))
		for _, td := range opts.Tools {
			tools = append(tools, &model.Tool{
				Type: "function",
				Function: &model.FunctionDefinition{
					Name:        td.Name,
					Description: td.Description,
					Parameters:  td.InputSchema,
				},
			})
		}
		req.Tools = tools
	}
	if opts.ToolChoice != nil {
		switch opts.ToolChoice.Type {
		case llm.ToolChoiceAuto:
			req.ToolChoice = "auto"
		case llm.ToolChoiceNone:
			req.ToolChoice = "none"
		case llm.ToolChoiceRequired:
			req.ToolChoice = "required"
		case llm.ToolChoiceSpecific:
			req.ToolChoice = model.ToolChoice{
				Type:     "function",
				Function: model.ToolChoiceFunction{Name: opts.ToolChoice.Name},
			}
		}
	}

	return req
}
