// Package bytedance provides the ByteDance Doubao LLM provider using
// the Volcengine ArkRuntime Go SDK.
//
// # Prompt caching
//
// Doubao implements automatic prefix caching server-side. The
// Responses API exposes system/developer text through `instructions`;
// routing locality is governed by Doubao's backend because ArkRuntime
// does not expose a routing-hint field analogous to OpenAI's
// `prompt_cache_key` or an explicit `cache_control` breakpoint.
//
// # Provider-specific Extra keys
//
// The Responses API surface is larger than the provider-agnostic
// [llm.GenerateOptions] exposes. The adapter honours these per-call
// keys via [llm.WithExtra]:
//
//   - "previous_response_id" (string): reference a prior stored
//     response to continue a conversation server-side. The referenced
//     response must have been created with store=true (see below).
//   - "store" (bool): whether the server stores this response for
//     later retrieval via previous_response_id. Defaults to false —
//     the adapter never reads responses back, so storing them only
//     burns server-side quota. Set true only on the response you
//     intend to continue.
//   - "thinking" (string): "auto" | "enabled" | "disabled". Overrides
//     [llm.WithThinking] and reaches the "auto" mode the bool option
//     cannot express. When neither is set the field is omitted so the
//     server applies its own default.
//   - "reasoning_effort" (string): "minimal" | "low" | "medium" |
//     "high". Maps to the Responses API `reasoning.effort` field;
//     only meaningful when thinking is enabled/auto.
//   - "web_search" (bool | map): enables the built-in web_search tool
//     (also configurable at provider construction).
//   - "parallel_tool_calls" (bool): forwarded to the Responses API.
package bytedance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
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
		model, err := New(modelName, apiKey, baseURL, region, retryTimes)
		if err != nil {
			return nil, err
		}
		model.webSearch = parseWebSearchConfig(config["web_search"])
		return model, nil
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
	// use are first-class. The Responses API supports streaming and
	// structured output natively, but does not expose stop sequences
	// or frequency/presence penalties.
	//
	// Output modality: text only. Image generation lives in
	// dedicated Doubao image SKUs and audio/video in Seedance 2.0 —
	// separate adapters not catalogued here. Disable the matching
	// output modality caps so policy matching does not route
	// image-output / audio-output slots onto these chat models.
	chatTextOutputOnly := llm.DisabledCaps(
		llm.CapImageOutput, llm.CapAudioOutput,
		llm.CapStopWords, llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		llm.CapAudio,
	)
	// Pro and Lite fix temperature=1 and top_p=0.95 server-side and
	// silently ignore manual values (per the Doubao Seed 2.0
	// Responses API reference). Disable those caps on those two SKUs
	// so the caps middleware fails fast instead of letting callers
	// believe their sampling params took effect. Mini honours
	// temperature/top_p, so it keeps the looser caps above.
	chatTextOutputOnlyFixedSampling := llm.DisabledCaps(
		llm.CapImageOutput, llm.CapAudioOutput,
		llm.CapStopWords, llm.CapFrequencyPenalty, llm.CapPresencePenalty,
		llm.CapAudio,
		llm.CapTemperature, llm.CapTopP,
	)

	llm.RegisterProviderModels("bytedance", []llm.ModelInfo{
		{
			Label: "Doubao Seed 2.0 Pro",
			Name:  "doubao-seed-2-0-pro-260215",
			Spec: llm.ModelSpec{
				Caps: chatTextOutputOnlyFixedSampling,
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
				Caps: chatTextOutputOnlyFixedSampling,
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
				Caps: chatTextOutputOnly,
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
			Spec: llm.ModelSpec{
				Caps: chatTextOutputOnly,
			},
		},
	})
}

// LLM implements llm.LLM using the Volcengine ArkRuntime Go SDK.
type LLM struct {
	client    *arkruntime.Client
	model     string
	webSearch webSearchConfig
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
	// http.DefaultClient has no Timeout and its DefaultTransport has
	// no ResponseHeaderTimeout, so a server that accepts the TCP
	// connection but never responds hangs forever when the caller
	// forgot a context deadline. Clone the default transport (keeps
	// its sane dial/TLS/idle timeouts) and add a response-header
	// ceiling so a stuck connection fails fast. We deliberately do
	// NOT set an overall Client.Timeout: legitimate streaming and
	// long thinking runs can last minutes, and the context is the
	// right cancellation channel for those.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	ropts = append(ropts, arkruntime.WithHTTPClient(&http.Client{Transport: transport}))

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
	req, err := c.buildRequest(messages, options)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return llm.Message{}, llm.TokenUsage{}, err
	}

	start := time.Now()
	resp, err := c.client.CreateResponses(ctx, req)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("bytedance.generate: %s", dur.String())
		}
		return llm.Message{}, llm.TokenUsage{}, classifyAPIError(err)
	}
	// Same nil-(resp,err) defensive guard as the OpenAI adapter: the
	// ark SDK's pointer-return convention is not a language guarantee,
	// and a 200 with an undecodable body has been seen returning
	// (nil, nil) on OpenAI-compatible backends. responseMessage is
	// nil-safe on its own, but classifying explicitly keeps the error
	// accurate and the two providers symmetric.
	if resp == nil {
		err := errdefs.NotAvailablef("bytedance: nil response with no error (provider misbehaviour)")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	msg := responseMessage(resp)
	if len(msg.Parts) == 0 {
		err := errdefs.NotAvailablef("bytedance: empty response")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	usage := responseUsage(resp)

	span.SetAttributes(llm.UsageSpanAttrs(usage)...)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "bytedance", c.model, "success", dur, usage)

	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.bytedance.stream.%s", c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, "bytedance"),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))

	options := llm.ApplyOptions(opts...)
	req, err := c.buildRequest(messages, options)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	stream, err := c.client.CreateResponsesStream(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", 0, llm.TokenUsage{})
		return nil, classifyAPIError(err)
	}
	// Defensive: the ark SDK has not been observed returning a nil
	// stream alongside a nil error, but the pointer-return convention
	// is not a language guarantee. Guarding keeps every provider on
	// the same contract.
	if stream == nil {
		err := errdefs.NotAvailablef("bytedance: nil stream handle with no error (provider misbehaviour)")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		llm.RecordLLMMetrics(ctx, "bytedance", c.model, "error", 0, llm.TokenUsage{})
		return nil, err
	}

	return newResponsesStreamMessage(ctx, span, c.model, stream), nil
}

func (c *LLM) buildRequest(msgs []llm.Message, opts *llm.GenerateOptions) (*arkresponses.ResponsesRequest, error) {
	maxTokens := defaultMaxTokens
	if opts.MaxTokens != nil {
		maxTokens = int(*opts.MaxTokens)
	}
	maxOutputTokens := int64(maxTokens)

	req := &arkresponses.ResponsesRequest{
		Model:           c.model,
		Input:           &arkresponses.ResponsesInput{Union: &arkresponses.ResponsesInput_ListValue{ListValue: &arkresponses.InputItemList{}}},
		MaxOutputTokens: &maxOutputTokens,
	}

	if opts.Temperature != nil {
		req.Temperature = opts.Temperature
	}
	if opts.TopP != nil {
		req.TopP = opts.TopP
	}
	if opts.JSONMode != nil && *opts.JSONMode {
		req.Text = &arkresponses.ResponsesText{Format: &arkresponses.TextFormat{Type: arkresponses.TextType_json_object}}
	}
	if opts.JSONSchema != nil {
		schema, err := json.Marshal(opts.JSONSchema.Schema)
		if err != nil {
			return nil, errdefs.Validation(errdefs.Fmt("bytedance: marshal json schema: %w", err))
		}
		req.Text = &arkresponses.ResponsesText{Format: &arkresponses.TextFormat{
			Type:        arkresponses.TextType_json_schema,
			Name:        opts.JSONSchema.Name,
			Description: stringPtrIfNotEmpty(opts.JSONSchema.Description),
			Schema:      &arkresponses.Bytes{Value: schema},
			Strict:      &opts.JSONSchema.Strict,
		}}
	}
	if len(opts.Tools) > 0 {
		tools := make([]*arkresponses.ResponsesTool, 0, len(opts.Tools))
		for _, td := range opts.Tools {
			schema, err := json.Marshal(td.InputSchema)
			if err != nil {
				return nil, errdefs.Validation(errdefs.Fmt("bytedance: marshal tool schema %q: %w", td.Name, err))
			}
			tools = append(tools, &arkresponses.ResponsesTool{Union: &arkresponses.ResponsesTool_ToolFunction{
				ToolFunction: &arkresponses.ToolFunction{
					Type:        arkresponses.ToolType_function,
					Name:        td.Name,
					Description: stringPtrIfNotEmpty(td.Description),
					Parameters:  &arkresponses.Bytes{Value: schema},
				},
			}})
		}
		req.Tools = tools
	}
	if opts.ToolChoice != nil {
		switch opts.ToolChoice.Type {
		case llm.ToolChoiceAuto:
			req.ToolChoice = &arkresponses.ResponsesToolChoice{Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_auto}}
		case llm.ToolChoiceNone:
			req.ToolChoice = &arkresponses.ResponsesToolChoice{Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_none}}
		case llm.ToolChoiceRequired:
			req.ToolChoice = &arkresponses.ResponsesToolChoice{Union: &arkresponses.ResponsesToolChoice_Mode{Mode: arkresponses.ToolChoiceMode_required}}
		case llm.ToolChoiceSpecific:
			req.ToolChoice = &arkresponses.ResponsesToolChoice{Union: &arkresponses.ResponsesToolChoice_FunctionToolChoice{
				FunctionToolChoice: &arkresponses.FunctionToolChoice{Type: arkresponses.ToolType_function, Name: opts.ToolChoice.Name},
			}}
		}
	}

	if v, ok := opts.Extra["parallel_tool_calls"].(bool); ok {
		req.ParallelToolCalls = &v
	}
	webSearch := c.webSearch
	if opts.Extra != nil {
		if v, ok := opts.Extra["web_search"]; ok {
			webSearch = parseWebSearchConfig(v)
		}
	}
	if webSearch.Enabled {
		req.Tools = append(req.Tools, webSearch.tool())
	}

	// Responses API fields the provider-agnostic options cannot express
	// (store, previous_response_id, thinking mode, reasoning effort).
	// See the package doc for the supported Extra keys.
	applyResponsesExtra(req, opts)

	if err := appendResponsesMessages(req, msgs); err != nil {
		return nil, err
	}
	if req.Instructions == nil && len(req.Input.GetListValue().GetListValue()) == 0 {
		return nil, errdefs.Validation(errdefs.New("bytedance: empty prompt"))
	}
	return req, nil
}
