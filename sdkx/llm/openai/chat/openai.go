package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	openaishared "github.com/GizClaw/flowcraft/sdkx/llm/openai/shared"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultModel is the fallback when callers create an OpenAI LLM
// without an explicit model id. We pick gpt-5 (not gpt-5.5) to stay
// on the cheapest current-generation SKU and avoid silently routing
// production traffic onto the flagship tier.
//
// Note: this used to be "gpt-4o", which was API-sunset on 2026-02-16
// per openai.com/index/retiring-gpt-4o-and-older-models/ — leaving
// it pointed there meant zero-config callers got hard 404s.
const defaultModel = openaishared.DefaultModel

// LLM implements llm.LLM using openai-go.
type LLM struct {
	client *oai.Client
	model  string

	// provider is the tag that lands on OTel spans, metrics, and the
	// fallback path of [classifyAPIError]'s error wrapping. It defaults
	// to "openai" so direct callers see the historical behaviour; the
	// sibling adapter packages (sdkx/llm/{azure,deepseek,qwen}) call
	// [LLM.WithProviderName] to override it so their traffic shows up
	// under their own name in observability tooling instead of being
	// silently aggregated under "openai". Same story on the Anthropic
	// side via sdkx/llm/minimax.
	provider string
}

var _ llm.LLM = (*LLM)(nil)

// defaultProviderName is the OTel/metrics tag stamped on every direct
// openai.New call. Wrapping adapters override it via WithProviderName.
const defaultProviderName = openaishared.DefaultProviderName

// New creates an OpenAI LLM instance.
func New(model, apiKey, baseURL string, extraOpts ...option.RequestOption) (*LLM, error) {
	if model == "" {
		model = defaultModel
	}
	client := openaishared.NewClient(apiKey, baseURL, extraOpts...)
	return &LLM{client: client, model: model, provider: defaultProviderName}, nil
}

// WithProviderName overrides the OTel / metrics provider tag used by
// this LLM instance. Wrapping adapters (sdkx/llm/azure, deepseek, qwen,
// ...) call this so each sub-provider's calls land under its own
// name in traces and metric labels instead of being aggregated under
// generic "openai". Returns the receiver for chaining; safe to ignore
// the return value. Empty names are silently ignored to keep the
// default intact when a caller passes an unset config.
func (c *LLM) WithProviderName(name string) *LLM {
	if c != nil && name != "" {
		c.provider = name
	}
	return c
}

// Provider returns the OTel / metrics tag used by this instance. Mostly
// a debugging aid; exported so eval drivers and observability dashboards
// can introspect what name they'll see in traces.
func (c *LLM) Provider() string {
	if c == nil || c.provider == "" {
		return defaultProviderName
	}
	return c.provider
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.generate.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	params, err := c.buildParams(messages, options)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}

	start := time.Now()
	resp, err := c.client.Chat.Completions.New(ctx, params, openaishared.ExtraRequestOpts(options)...)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.FromContext(fmt.Errorf("%s.generate: %s: %w", provider, dur.String(), err))
		}
		return llm.Message{}, llm.TokenUsage{}, openaishared.ClassifyAPIErrorWithProvider(c.Provider(), err)
	}
	// openai-go and OpenAI-compatible backends (deepseek, qwen-flash on
	// busy hours, self-hosted) have been observed returning (nil, nil)
	// in the wild — HTTP 200 with an empty body, decoder failure that
	// gets swallowed, or internal retry+timeout races. The Go pointer-
	// return convention "err==nil ⇒ resp!=nil" is *not* a language-level
	// guarantee, so dereferencing without checking would crash the whole
	// runner. Classify as NotAvailable so upstream retry logic treats it
	// like any other transient provider misbehaviour.
	if resp == nil {
		err := errdefs.NotAvailablef("%s: nil response with no error (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	if len(resp.Choices) == 0 {
		err := errdefs.NotAvailablef("%s: no choices returned", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, provider, c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	msg := convertResponse(resp.Choices[0].Message)
	usage := llm.TokenUsage{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		// usage.prompt_tokens_details.cached_tokens is the subset of
		// PromptTokens the provider served from its prefix cache.
		// OpenAI / Azure OpenAI / Qwen-flash populate it when the wire
		// response carries prompt_tokens_details; older / lighter
		// compatibles leave it zero. DeepSeek reports the same value as
		// a top-level prompt_cache_hit_tokens, so we fall back to the
		// raw response when the nested field is absent.
		CachedInputTokens: openaishared.CachedInputTokensFromUsage(resp.Usage),
	}

	span.SetAttributes(llm.UsageSpanAttrs(usage)...)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, provider, c.model, "success", dur, usage)

	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	provider := c.Provider()
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.%s.stream.%s", provider, c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, provider),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))

	options := llm.ApplyOptions(opts...)
	params, err := c.buildParams(messages, options)
	if err != nil {
		span.End()
		return nil, err
	}
	params.StreamOptions = oai.ChatCompletionStreamOptionsParam{
		IncludeUsage: oai.Bool(true),
	}

	reqOpts := openaishared.ExtraRequestOpts(options)
	start := time.Now()
	stream := c.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
	// NewStreaming may return nil if the SDK fails before allocating the
	// SSE handle (HTTP transport dial failure, panic recovery, etc.).
	// The non-streaming Generate has the same family of failure modes —
	// see the resp==nil nil-check above. Guard both stream-handle
	// allocations symmetrically so a flaky provider can't take down
	// the eval runner.
	if stream == nil {
		err := errdefs.NotAvailablef("%s: nil stream handle (provider misbehaviour)", provider)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	if err := stream.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errdefs.FromContext(fmt.Errorf("%s.stream: %s: %w: %w", provider, time.Since(start).String(), err, ctxErr))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return nil, err
		}
		// Some OpenAI-compatible providers don't support stream_options; retry without it.
		params.StreamOptions = oai.ChatCompletionStreamOptionsParam{}
		stream = c.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
		if stream == nil {
			err := errdefs.NotAvailablef("%s: nil stream handle on retry without stream_options", provider)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return nil, err
		}
		if err2 := stream.Err(); err2 != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				err2 = errdefs.FromContext(fmt.Errorf("%s.stream: %s: %w: %w", provider, time.Since(start).String(), err2, ctxErr))
				span.RecordError(err2)
				span.SetStatus(codes.Error, err2.Error())
				span.End()
				return nil, err2
			}
			span.RecordError(err2)
			span.SetStatus(codes.Error, err2.Error())
			span.End()
			return nil, openaishared.ClassifyAPIErrorWithProvider(c.Provider(), err2)
		}
	}

	return newStreamMessage(ctx, span, provider, c.model, stream), nil
}

func (c *LLM) buildParams(msgs []llm.Message, opts *llm.GenerateOptions) (oai.ChatCompletionNewParams, error) {
	converted, err := convertMessages(msgs)
	if err != nil {
		return oai.ChatCompletionNewParams{}, err
	}
	params := oai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: converted,
	}

	if opts.Temperature != nil {
		params.Temperature = oai.Float(*opts.Temperature)
	}
	if opts.MaxTokens != nil {
		params.MaxCompletionTokens = oai.Int(*opts.MaxTokens)
	}
	if opts.TopP != nil {
		params.TopP = oai.Float(*opts.TopP)
	}
	if opts.FrequencyPenalty != nil {
		params.FrequencyPenalty = oai.Float(*opts.FrequencyPenalty)
	}
	if opts.PresencePenalty != nil {
		params.PresencePenalty = oai.Float(*opts.PresencePenalty)
	}
	if len(opts.StopWords) > 0 {
		if len(opts.StopWords) == 1 {
			params.Stop = oai.ChatCompletionNewParamsStopUnion{
				OfString: oai.String(opts.StopWords[0]),
			}
		} else {
			params.Stop = oai.ChatCompletionNewParamsStopUnion{
				OfStringArray: opts.StopWords,
			}
		}
	}
	if opts.JSONSchema != nil {
		schemaParam := oai.ResponseFormatJSONSchemaJSONSchemaParam{
			Name:   opts.JSONSchema.Name,
			Schema: opts.JSONSchema.Schema,
		}
		if opts.JSONSchema.Description != "" {
			schemaParam.Description = oai.String(opts.JSONSchema.Description)
		}
		if opts.JSONSchema.Strict {
			schemaParam.Strict = oai.Bool(true)
		}
		params.ResponseFormat = oai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &oai.ResponseFormatJSONSchemaParam{
				JSONSchema: schemaParam,
			},
		}
	} else if opts.JSONMode != nil && *opts.JSONMode {
		params.ResponseFormat = oai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &oai.ResponseFormatJSONObjectParam{},
		}
	}
	if len(opts.Tools) > 0 {
		params.Tools = convertTools(opts.Tools)
	}
	if opts.ToolChoice != nil {
		params.ToolChoice = convertToolChoice(*opts.ToolChoice)
	}
	// Auto-inject `prompt_cache_key` derived from the
	// cache-eligible prefix (system messages + tool definitions) so
	// requests with identical stable parts land on the same backend
	// node consistently — flipping implicit prompt-cache hit rate
	// from "round-robin lottery" to "deterministic hit when the
	// prefix is identical". See sdkx/llm/openai/cache.go for the
	// derivation rationale and what's excluded (message history is
	// turn-varying, so feeding it into the key would defeat the
	// purpose). Caller can override by passing
	// llm.WithExtra("prompt_cache_key", "custom") which buildParams
	// honours via the extraRequestOpts path used downstream.
	if key := openaishared.ComputePromptCacheKey(msgs, opts.Tools); key != "" {
		params.PromptCacheKey = oai.String(key)
	}

	return params, nil
}

func convertMessages(msgs []llm.Message) ([]oai.ChatCompletionMessageParamUnion, error) {
	out := make([]oai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleSystem:
			if err := openaishared.ValidateSystemTextParts(msg.Parts); err != nil {
				return nil, err
			}
			out = append(out, oai.SystemMessage(msg.Content()))

		case llm.RoleUser:
			var parts []oai.ChatCompletionContentPartUnionParam
			for _, p := range msg.Parts {
				switch p.Type {
				case llm.PartText:
					parts = append(parts, oai.TextContentPart(p.Text))
				case llm.PartImage:
					if p.Image != nil {
						url := mediaURL(p.Image)
						if url == "" {
							continue
						}
						parts = append(parts, oai.ImageContentPart(oai.ChatCompletionContentPartImageImageURLParam{
							URL: url,
						}))
					}
				case llm.PartFile:
					if p.File != nil {
						parts = append(parts, convertFilePartOpenAI(p.File))
					}
				case llm.PartData:
					if p.Data != nil {
						text, err := openaishared.FormatDataPartText(p.Data)
						if err != nil {
							return nil, err
						}
						parts = append(parts, oai.TextContentPart(text))
					}
				case llm.PartAudio:
					if p.Audio != nil {
						return nil, errdefs.Validationf("openai chat: role user does not support part type %s", p.Type)
					}
				case llm.PartToolCall:
					if p.ToolCall != nil {
						return nil, errdefs.Validationf("openai chat: role user does not support part type %s", p.Type)
					}
				case llm.PartToolResult:
					if p.ToolResult != nil {
						return nil, errdefs.Validationf("openai chat: role user does not support part type %s", p.Type)
					}
				default:
					if p.Type != "" {
						return nil, errdefs.Validationf("openai chat: role user does not support part type %s", p.Type)
					}
				}
			}
			if len(parts) > 0 {
				out = append(out, oai.UserMessage(parts))
			} else {
				out = append(out, oai.UserMessage(msg.Content()))
			}

		case llm.RoleAssistant:
			content, err := openaishared.TextContent(msg.Parts)
			if err != nil {
				return nil, err
			}
			if msg.HasToolCalls() {
				calls := msg.ToolCalls()
				sdkCalls := make([]oai.ChatCompletionMessageToolCallParam, len(calls))
				for i, tc := range calls {
					sdkCalls[i] = oai.ChatCompletionMessageToolCallParam{
						ID: tc.ID,
						Function: oai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: tc.Arguments,
						},
					}
				}
				var assistant oai.ChatCompletionAssistantMessageParam
				assistant.ToolCalls = sdkCalls
				if content != "" {
					assistant.Content.OfString = oai.String(content)
				}
				out = append(out, oai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			} else {
				out = append(out, oai.AssistantMessage(content))
			}

		case llm.RoleTool:
			for _, r := range msg.ToolResults() {
				out = append(out, oai.ToolMessage(r.Content, r.ToolCallID))
			}
		}
	}
	return out, nil
}

func convertResponse(msg oai.ChatCompletionMessage) llm.Message {
	var parts []llm.Part
	if strings.TrimSpace(msg.Content) != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		parts = append(parts, llm.Part{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func convertTools(tools []llm.ToolDefinition) []oai.ChatCompletionToolParam {
	out := make([]oai.ChatCompletionToolParam, len(tools))
	for i, t := range tools {
		out[i] = oai.ChatCompletionToolParam{
			Function: oai.FunctionDefinitionParam{
				Name:        t.Name,
				Description: oai.String(t.Description),
				Parameters:  oai.FunctionParameters(t.InputSchema),
			},
		}
	}
	return out
}

func convertToolChoice(tc llm.ToolChoice) oai.ChatCompletionToolChoiceOptionUnionParam {
	switch tc.Type {
	case llm.ToolChoiceAuto:
		return oai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: oai.String("auto")}
	case llm.ToolChoiceNone:
		return oai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: oai.String("none")}
	case llm.ToolChoiceRequired:
		return oai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: oai.String("required")}
	case llm.ToolChoiceSpecific:
		return oai.ChatCompletionToolChoiceOptionUnionParam{
			OfChatCompletionNamedToolChoice: &oai.ChatCompletionNamedToolChoiceParam{
				Function: oai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc.Name},
			},
		}
	default:
		return oai.ChatCompletionToolChoiceOptionUnionParam{}
	}
}

func convertFilePartOpenAI(f *llm.FileRef) oai.ChatCompletionContentPartUnionParam {
	if strings.HasPrefix(f.MimeType, "image/") {
		return oai.ImageContentPart(oai.ChatCompletionContentPartImageImageURLParam{URL: f.URI})
	}
	fp := oai.ChatCompletionContentPartFileFileParam{}
	if strings.HasPrefix(f.URI, "file-") {
		fp.FileID = param.NewOpt(f.URI)
	} else {
		fp.FileData = param.NewOpt(f.URI)
	}
	if f.Name != "" {
		fp.Filename = param.NewOpt(f.Name)
	}
	return oai.FileContentPart(fp)
}

func mediaURL(media *llm.MediaRef) string {
	if media.URL != "" {
		return media.URL
	}
	if media.Base64 == "" {
		return ""
	}
	mime := media.MediaType
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + media.Base64
}
