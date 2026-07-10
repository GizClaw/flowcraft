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
		c, err := New(model, baseURL)
		if err != nil {
			return nil, err
		}
		// Ollama is a local-model runner: the actual capability set
		// depends on whichever model the user has pulled. We cannot
		// predeclare per-model caps, but we apply a provider-level
		// safe default that disables features the Ollama /api/chat
		// protocol itself does not support, so callers get a clear
		// fail-fast instead of a silent drop.
		return llm.WithCaps(c, defaultOllamaCaps), nil
	})
}

// LLM implements llm.LLM using Ollama's native HTTP API.
type LLM struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

var _ llm.LLM = (*LLM)(nil)

// defaultOllamaCaps is the capability set applied to every Ollama model
// by New. Because Ollama models are user-defined and the registry does not
// know their names, we cannot look up per-model caps; instead we apply the
// provider-level safe default here.
var defaultOllamaCaps = llm.DisabledCaps(
	llm.CapAudio,
	llm.CapFile,
	llm.CapJSONSchema,
	llm.CapToolChoice,
)

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
		httpClient: defaultHTTPClient(),
	}, nil
}

// defaultHTTPClient returns an http.Client with a ResponseHeaderTimeout
// safety net so a hung local Ollama server cannot block a caller
// indefinitely when no context deadline is set. The overall operation is
// still governed by the caller's context.
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	return &http.Client{Transport: transport}
}

func (c *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.ollama.generate.%s", c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, "ollama"),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))
	defer span.End()

	options := llm.ApplyOptions(opts...)
	msgs, err := convertMessages(messages)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return llm.Message{}, llm.TokenUsage{}, err
	}

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
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(fmt.Errorf("ollama: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(fmt.Errorf("ollama: %w", err))
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
		return llm.Message{}, llm.TokenUsage{}, errdefs.ClassifyProviderError("ollama", err)
	}
	// net/http's documented contract is "err==nil ⇒ resp!=nil", but
	// callers can inject a custom *http.Client whose RoundTripper
	// violates it (middleware, recording proxies). Guard symmetrically
	// with the other providers so a misbehaving transport surfaces a
	// clean error instead of a nil-deref panic.
	if resp == nil {
		err := errdefs.NotAvailable(fmt.Errorf("ollama: nil http response with no error (custom transport misbehaviour)"))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "ollama", c.model, "error", dur, llm.TokenUsage{})
		return llm.Message{}, llm.TokenUsage{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.NotAvailable(fmt.Errorf("ollama: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return llm.Message{}, llm.TokenUsage{}, errdefs.ClassifyHTTPStatus("ollama", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var out chatResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return llm.Message{}, llm.TokenUsage{}, errdefs.Validation(fmt.Errorf("ollama: decode response: %w", err))
	}

	msg := convertOllamaResponse(out.Message)
	usage := llm.TokenUsage{
		InputTokens:  out.PromptEvalCount,
		OutputTokens: out.EvalCount,
		TotalTokens:  out.PromptEvalCount + out.EvalCount,
	}

	span.SetAttributes(
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens),
	)
	span.SetStatus(codes.Ok, "OK")
	llm.RecordLLMMetrics(ctx, "ollama", c.model, "success", dur, usage)
	return msg, usage, nil
}

func (c *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("llm.ollama.stream.%s", c.model), trace.WithAttributes(
		attribute.String(telemetry.AttrLLMProvider, "ollama"),
		attribute.String(telemetry.AttrLLMModel, c.model),
	))

	options := llm.ApplyOptions(opts...)
	msgs, err := convertMessages(messages)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

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
		return nil, errdefs.Validation(fmt.Errorf("ollama: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		span.End()
		return nil, errdefs.Validation(fmt.Errorf("ollama: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "ollama", c.model, "error", dur, llm.TokenUsage{})
		span.End()
		if ctx.Err() != nil {
			return nil, errdefs.FromContext(ctx.Err())
		}
		return nil, errdefs.ClassifyProviderError("ollama", err)
	}
	// See nil-resp guard rationale on the non-streaming path above.
	if resp == nil {
		err := errdefs.NotAvailable(fmt.Errorf("ollama: nil http response with no error (custom transport misbehaviour)"))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(ctx, "ollama", c.model, "error", dur, llm.TokenUsage{})
		span.End()
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		bodyBytes, _ := io.ReadAll(resp.Body)
		statusErr := errdefs.ClassifyHTTPStatus("ollama", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		span.RecordError(statusErr)
		span.SetStatus(codes.Error, statusErr.Error())
		llm.RecordLLMMetrics(ctx, "ollama", c.model, "error", dur, llm.TokenUsage{})
		span.End()
		return nil, statusErr
	}

	return newStreamMessage(ctx, span, c.model, resp.Body), nil
}

// --- helpers ---

func convertMessages(messages []llm.Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case llm.RoleTool:
			for _, r := range m.ToolResults() {
				out = append(out, chatMessage{Role: "tool", Content: r.Content})
			}
		case llm.RoleSystem:
			var b strings.Builder
			for _, p := range m.Parts {
				if p.Type != llm.PartText {
					return nil, errdefs.Validationf("ollama: system message supports text parts only, got %s", p.Type)
				}
				b.WriteString(p.Text)
			}
			out = append(out, chatMessage{Role: "system", Content: b.String()})
		case llm.RoleAssistant:
			text, images, err := convertContentParts(m.Parts)
			if err != nil {
				return nil, err
			}
			if m.HasToolCalls() {
				msg := chatMessage{Role: "assistant", Content: text}
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
				msg := chatMessage{Role: "assistant", Content: text}
				if len(images) > 0 {
					msg.Images = images
				}
				out = append(out, msg)
			}
		default:
			text, images, err := convertContentParts(m.Parts)
			if err != nil {
				return nil, err
			}
			msg := chatMessage{Role: string(m.Role), Content: text}
			if len(images) > 0 {
				msg.Images = images
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

func convertContentParts(parts []llm.Part) (text string, images []string, err error) {
	var b strings.Builder
	needsTextBoundary := false
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if needsTextBoundary && p.Text != "" {
				ensureOllamaContentBoundary(&b)
			}
			b.WriteString(p.Text)
			needsTextBoundary = false
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
				needsTextBoundary = false
			}
		case llm.PartData:
			if p.Data != nil {
				raw, err := json.Marshal(p.Data.Value)
				if err != nil {
					return "", nil, errdefs.Validationf("ollama: marshal data part: %w", err)
				}
				ensureOllamaContentBoundary(&b)
				b.WriteString("[ollama data]\n")
				b.WriteString("mime_type: ")
				mime := strings.TrimSpace(p.Data.MimeType)
				if mime == "" {
					mime = "application/json"
				}
				b.WriteString(mime)
				b.WriteString("\njson:\n")
				b.Write(raw)
				b.WriteString("\n[/ollama data]")
				needsTextBoundary = true
			}
		}
	}
	return b.String(), images, nil
}

func ensureOllamaContentBoundary(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	s := b.String()
	switch {
	case strings.HasSuffix(s, "\n\n"):
		return
	case strings.HasSuffix(s, "\n"):
		b.WriteByte('\n')
	default:
		b.WriteString("\n\n")
	}
}

func normalizeImageToBase64(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errdefs.Validationf("ollama: empty image")
	}
	if strings.HasPrefix(s, "data:") {
		i := strings.Index(s, ";base64,")
		if i < 0 {
			return "", errdefs.Validationf("ollama: invalid data url (missing ;base64,)")
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
