package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const defaultBaseURL = "http://127.0.0.1:11434"

func init() {
	llm.RegisterProvider("ollama", func(model string, config map[string]any) (llm.LLM, error) {
		baseURL, _ := config["base_url"].(string)
		return New(model, baseURL)
	})
}

// LLM implements llm.LLM using Ollama's native HTTP API.
type LLM struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

var _ llm.LLM = (*LLM)(nil)

// New creates an Ollama LLM instance.
func New(model, baseURL string) (*LLM, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	} else {
		baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
	return &LLM{
		baseURL:    baseURL,
		model:      model,
		httpClient: http.DefaultClient,
	}, nil
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.ollama.generate.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "ollama"),
		attribute.String("llm.model", c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	msgs := convertMessages(messages)

	if strings.TrimSpace(c.model) == "" {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validationf("ollama: model is required")
	}

	req := chatRequest{
		Model:    c.model,
		Messages: msgs,
		Stream:   false,
		Think:    options.Thinking,
	}
	applyGenerateOptions(&req, options)

	b, err := json.Marshal(req)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "ollama", c.model, "error", dur, llm.TokenUsage{})
		if ctx.Err() != nil {
			return llm.Message{}, llm.TokenUsage{}, errdefs.Timeoutf("ollama.generate: %s", dur.String())
		}
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: http %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var out chatResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return llm.Message{}, llm.TokenUsage{}, fmt.Errorf("ollama: decode response: %w", err)
	}

	msg := convertOllamaResponse(out.Message)
	usage := llm.TokenUsage{
		InputTokens:  out.PromptEvalCount,
		OutputTokens: out.EvalCount,
		TotalTokens:  out.PromptEvalCount + out.EvalCount,
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", usage.InputTokens),
		attribute.Int64("llm.output_tokens", usage.OutputTokens),
	)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "ollama", c.model, "success", dur, usage)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.ollama.stream.%s", c.model), trace.WithAttributes(
		attribute.String("llm.provider", "ollama"),
		attribute.String("llm.model", c.model),
	))

	options := llm.ApplyOptions(opts...)
	msgs := convertMessages(messages)

	if strings.TrimSpace(c.model) == "" {
		span.End()
		return nil, errdefs.Validationf("ollama: model is required")
	}

	req := chatRequest{
		Model:    c.model,
		Messages: msgs,
		Stream:   true,
		Think:    options.Thinking,
	}
	applyGenerateOptions(&req, options)

	b, err := json.Marshal(req)
	if err != nil {
		span.End()
		return nil, fmt.Errorf("ollama: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		span.End()
		return nil, fmt.Errorf("ollama: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		span.End()
		return nil, fmt.Errorf("ollama: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		bodyBytes, _ := io.ReadAll(resp.Body)
		span.End()
		return nil, fmt.Errorf("ollama: http %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	return newStreamMessage(ctx, span, c.model, resp.Body), nil
}

// --- helpers ---

func convertMessages(messages []llm.Message) []chatMessage {
	out := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case llm.RoleTool:
			for _, r := range m.ToolResults() {
				out = append(out, chatMessage{Role: "tool", Content: r.Content})
			}
		case llm.RoleAssistant:
			if m.HasToolCalls() {
				msg := chatMessage{Role: "assistant", Content: m.Content()}
				for _, tc := range m.ToolCalls() {
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
						args = map[string]any{"_raw": tc.Arguments}
					}
					msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
						Function: ollamaFunctionCall{Name: tc.Name, Arguments: args},
					})
				}
				out = append(out, msg)
			} else {
				out = append(out, chatMessage{Role: "assistant", Content: m.Content()})
			}
		default:
			text, images := convertContentParts(m.Parts)
			msg := chatMessage{Role: string(m.Role), Content: text}
			if len(images) > 0 {
				msg.Images = images
			}
			out = append(out, msg)
		}
	}
	return out
}

func convertContentParts(parts []llm.Part) (text string, images []string) {
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			b.WriteString(p.Text)
		case llm.PartImage:
			if p.Image == nil {
				continue
			}
			raw := strings.TrimSpace(p.Image.URL)
			if raw == "" {
				continue
			}
			img, err := normalizeImageToBase64(raw)
			if err != nil {
				continue
			}
			images = append(images, img)
		case llm.PartFile:
			if p.File != nil && strings.HasPrefix(p.File.MimeType, "image/") {
				if img, err := normalizeImageToBase64(p.File.URI); err == nil {
					images = append(images, img)
				}
			} else if p.File != nil {
				b.WriteString(p.File.URI)
			}
		case llm.PartData:
			if p.Data != nil {
				raw, _ := json.Marshal(p.Data.Value)
				b.Write(raw)
			}
		}
	}
	return b.String(), images
}

func normalizeImageToBase64(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("ollama: empty image")
	}
	if strings.HasPrefix(s, "data:") {
		i := strings.Index(s, ";base64,")
		if i < 0 {
			return "", fmt.Errorf("ollama: invalid data url (missing ;base64,)")
		}
		return s[i+len(";base64,"):], nil
	}
	return s, nil
}

func convertOllamaResponse(msg chatMessage) llm.Message {
	if len(msg.ToolCalls) > 0 {
		var parts []llm.Part
		if strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, llm.Part{Type: llm.PartText, Text: msg.Content})
		}
		for i, tc := range msg.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Function.Arguments)
			parts = append(parts, llm.Part{
				Type: llm.PartToolCall,
				ToolCall: &llm.ToolCall{
					ID:        fmt.Sprintf("call_%d", i),
					Name:      tc.Function.Name,
					Arguments: string(argsBytes),
				},
			})
		}
		return llm.Message{Role: llm.RoleAssistant, Parts: parts}
	}
	return llm.NewTextMessage(llm.RoleAssistant, msg.Content)
}

func applyGenerateOptions(req *chatRequest, opts *llm.GenerateOptions) {
	var o chatOptions
	set := false
	if opts.Temperature != nil {
		o.Temperature = opts.Temperature
		set = true
	}
	if opts.TopP != nil {
		o.TopP = opts.TopP
		set = true
	}
	if opts.TopK != nil {
		o.TopK = opts.TopK
		set = true
	}
	if opts.MaxTokens != nil {
		o.NumPredict = opts.MaxTokens
		set = true
	}
	if len(opts.StopWords) > 0 {
		o.Stop = opts.StopWords
		set = true
	}
	if opts.FrequencyPenalty != nil {
		o.Frequency = opts.FrequencyPenalty
		set = true
	}
	if opts.PresencePenalty != nil {
		o.Presence = opts.PresencePenalty
		set = true
	}
	if set {
		req.Options = &o
	}
	if opts.JSONMode != nil && *opts.JSONMode {
		req.Format = "json"
	}
	if len(opts.Tools) > 0 {
		tools := make([]ollamaTool, 0, len(opts.Tools))
		for _, td := range opts.Tools {
			tools = append(tools, ollamaTool{
				Type:     "function",
				Function: ollamaToolFunction{Name: td.Name, Description: td.Description, Parameters: td.InputSchema},
			})
		}
		req.Tools = tools
	}
}
