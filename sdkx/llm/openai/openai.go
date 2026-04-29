package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const defaultModel = "gpt-4o"

func init() {
	llm.RegisterProvider("openai", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		return New(model, apiKey, baseURL)
	})

	llm.RegisterProviderModels("openai", []llm.ModelInfo{
		{Label: "GPT-5.4", Name: "gpt-5.4"},
		{Label: "GPT-5.4 Pro", Name: "gpt-5.4-pro"},
		{Label: "GPT-5", Name: "gpt-5"},
		{Label: "GPT-5 nano", Name: "gpt-5-nano"},
		{Label: "GPT-5 Mini", Name: "gpt-5-mini"},
		{Label: "GPT-4.1", Name: "gpt-4.1"},
	})
}

// LLM implements llm.LLM using openai-go.
type LLM struct {
	client *oai.Client
	model  string
}

var _ llm.LLM = (*LLM)(nil)

// New creates an OpenAI LLM instance.
func New(model, apiKey, baseURL string, extraOpts ...option.RequestOption) (*LLM, error) {
	if model == "" {
		model = defaultModel
	}
	var clientOpts []option.RequestOption
	if apiKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(baseURL))
	}
	clientOpts = append(clientOpts, extraOpts...)
	client := oai.NewClient(clientOpts...)
	return &LLM{client: &client, model: model}, nil
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.openai.generate.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "openai"),
		attribute.String("llm.model", c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	params := c.buildParams(messages, options)

	start := time.Now()
	resp, err := c.client.Chat.Completions.New(ctx, params, extraRequestOpts(options)...)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "openai", c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("openai.generate: %s", dur.String())
		}
		return llm.Message{}, llm.TokenUsage{}, llm.ClassifyProviderError("openai", err)
	}

	if len(resp.Choices) == 0 {
		err := errdefs.NotAvailablef("openai: no choices returned")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "openai", c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}

	msg := convertResponse(resp.Choices[0].Message)
	usage := llm.TokenUsage{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", usage.InputTokens),
		attribute.Int64("llm.output_tokens", usage.OutputTokens),
	)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "openai", c.model, "success", dur, usage)

	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.openai.stream.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "openai"),
		attribute.String("llm.model", c.model),
	))

	options := llm.ApplyOptions(opts...)
	params := c.buildParams(messages, options)
	params.StreamOptions = oai.ChatCompletionStreamOptionsParam{
		IncludeUsage: oai.Bool(true),
	}

	reqOpts := extraRequestOpts(options)
	stream := c.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
	if err := stream.Err(); err != nil {
		// Some OpenAI-compatible providers don't support stream_options; retry without it.
		params.StreamOptions = oai.ChatCompletionStreamOptionsParam{}
		stream = c.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
		if err2 := stream.Err(); err2 != nil {
			span.RecordError(err2)
			span.SetStatus(codes.Error, err2.Error())
			span.End()
			return nil, llm.ClassifyProviderError("openai", err2)
		}
	}

	return newStreamMessage(ctx, span, c.model, stream), nil
}

// extraRequestOpts converts GenerateOptions.Extra into per-request
// option.WithJSONSet calls, allowing sub-providers (e.g. qwen) to inject
// arbitrary body fields via the standard Extra mechanism.
func extraRequestOpts(opts *llm.GenerateOptions) []option.RequestOption {
	if len(opts.Extra) == 0 {
		return nil
	}
	out := make([]option.RequestOption, 0, len(opts.Extra))
	for k, v := range opts.Extra {
		out = append(out, option.WithJSONSet(k, v))
	}
	return out
}

func (c *LLM) buildParams(msgs []llm.Message, opts *llm.GenerateOptions) oai.ChatCompletionNewParams {
	params := oai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: convertMessages(msgs),
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

	return params
}

func convertMessages(msgs []llm.Message) []oai.ChatCompletionMessageParamUnion {
	out := make([]oai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleSystem:
			out = append(out, oai.SystemMessage(msg.Content()))

		case llm.RoleUser:
			var parts []oai.ChatCompletionContentPartUnionParam
			for _, p := range msg.Parts {
				switch p.Type {
				case llm.PartText:
					parts = append(parts, oai.TextContentPart(p.Text))
				case llm.PartImage:
					if p.Image != nil && p.Image.URL != "" {
						parts = append(parts, oai.ImageContentPart(oai.ChatCompletionContentPartImageImageURLParam{
							URL: p.Image.URL,
						}))
					}
				case llm.PartFile:
					if p.File != nil {
						parts = append(parts, convertFilePartOpenAI(p.File))
					}
				case llm.PartData:
					if p.Data != nil {
						b, _ := json.Marshal(p.Data.Value)
						parts = append(parts, oai.TextContentPart(string(b)))
					}
				}
			}
			if len(parts) > 0 {
				out = append(out, oai.UserMessage(parts))
			} else {
				out = append(out, oai.UserMessage(msg.Content()))
			}

		case llm.RoleAssistant:
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
				if text := msg.Content(); text != "" {
					assistant.Content.OfString = oai.String(text)
				}
				out = append(out, oai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			} else {
				out = append(out, oai.AssistantMessage(msg.Content()))
			}

		case llm.RoleTool:
			for _, r := range msg.ToolResults() {
				out = append(out, oai.ToolMessage(r.Content, r.ToolCallID))
			}
		}
	}
	return out
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
