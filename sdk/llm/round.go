package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RoundResult is the structured output of one LLM round.
type RoundResult struct {
	Content     string
	Message     Message
	Messages    []Message
	ToolCalls   []model.ToolCall
	ToolResults []model.ToolResult
	ToolPending bool
	Usage       TokenUsage
}

// RunRound executes one LLM round: resolve model → stream generation → tool
// follow-up → return result. It is a pure function: callers supply messages
// explicitly and handle board I/O themselves.
//
// stream may be nil; when non-nil, token / tool_call / tool_result events are
// emitted as they arrive (eventID labels the source in each event).
func RunRound(
	ctx context.Context,
	stream workflow.StreamCallback,
	resolver LLMResolver,
	reg *tool.Registry,
	eventID string,
	messages []Message,
	cfg RoundConfig,
) (*RoundResult, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "llm.round",
		trace.WithAttributes(attribute.String("event.id", eventID)))
	defer span.End()

	s, err := StreamRound(ctx, stream, resolver, reg, eventID, messages, cfg)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	defer s.Close()

	for s.Next() {
	}
	result, err := s.Finish()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int64("llm.output_tokens", result.Usage.OutputTokens),
		attribute.Bool("llm.tool_pending", result.ToolPending),
		attribute.Int("llm.tool_calls", len(result.ToolCalls)),
	)
	return result, nil
}

// RoundStream is a token-by-token iterator over an in-progress LLM round.
// Callers loop with Next() / Token(), then call Finish() for the complete result.
type RoundStream struct {
	ctx     context.Context
	inner   StreamMessage
	reg     *tool.Registry
	stream  workflow.StreamCallback
	eventID string

	messages []Message
	current  string
	acc      strings.Builder
}

// StreamRound starts an LLM round and returns a RoundStream for
// token-by-token iteration. Call Finish() after Next() returns false.
func StreamRound(
	ctx context.Context,
	stream workflow.StreamCallback,
	resolver LLMResolver,
	reg *tool.Registry,
	eventID string,
	messages []Message,
	cfg RoundConfig,
) (*RoundStream, error) {
	l, err := resolver.Resolve(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: cannot resolve model %q: %w", eventID, cfg.Model, err)
	}

	opts := buildRoundGenerateOptions(cfg, reg)

	inner, err := l.GenerateStream(ctx, messages, opts...)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: generate failed: %w", eventID, err)
	}

	msgsCopy := make([]Message, len(messages))
	copy(msgsCopy, messages)

	return &RoundStream{
		ctx:      ctx,
		inner:    inner,
		reg:      reg,
		stream:   stream,
		eventID:  eventID,
		messages: msgsCopy,
	}, nil
}

// Next advances to the next token chunk. Returns false when the stream is done.
func (s *RoundStream) Next() bool {
	if !s.inner.Next() {
		return false
	}
	chunk := s.inner.Current()
	s.current = chunk.Content
	if s.current != "" {
		s.acc.WriteString(s.current)
		if s.stream != nil {
			s.stream(workflow.StreamEvent{
				Type:    "token",
				NodeID:  s.eventID,
				Payload: map[string]any{"content": s.current},
			})
		}
	}
	return true
}

// Token returns the content string of the current chunk.
func (s *RoundStream) Token() string { return s.current }

// Close releases the underlying stream resources. It is safe to call
// multiple times. Finish calls Close automatically on success; callers
// should defer Close when there is any chance of early return.
func (s *RoundStream) Close() error {
	inner := s.inner
	s.inner = nil
	if inner != nil {
		return inner.Close()
	}
	return nil
}

// Finish completes the round: checks stream error, executes tool calls (if any),
// and returns the full RoundResult. It closes the underlying stream on success.
func (s *RoundStream) Finish() (*RoundResult, error) {
	defer s.Close()

	if err := s.inner.Err(); err != nil {
		return nil, fmt.Errorf("llm round %q: stream error: %w", s.eventID, err)
	}

	rawUsage := s.inner.Usage()
	usage := TokenUsage{
		InputTokens:  rawUsage.InputTokens,
		OutputTokens: rawUsage.OutputTokens,
		TotalTokens:  rawUsage.InputTokens + rawUsage.OutputTokens,
	}

	accMsg := s.inner.Message()
	if s.acc.Len() > 0 && len(accMsg.Parts) == 0 {
		accMsg = NewTextMessage(RoleAssistant, s.acc.String())
	}

	messages := s.messages
	if accMsg.Role != "" || len(accMsg.Parts) > 0 {
		messages = append(messages, accMsg)
	}

	toolCalls := accMsg.ToolCalls()
	toolPending := false
	var toolResults []model.ToolResult

	if len(toolCalls) > 0 && s.reg != nil {
		toolPending = true

		if s.stream != nil {
			for _, tc := range toolCalls {
				s.stream(workflow.StreamEvent{
					Type:    "tool_call",
					NodeID:  s.eventID,
					Payload: map[string]any{"id": tc.ID, "name": tc.Name, "arguments": tc.Arguments},
				})
			}
		}

		toolResults = s.reg.ExecuteAll(s.ctx, toolCalls)

		if s.stream != nil {
			tcNames := make(map[string]string, len(toolCalls))
			for _, tc := range toolCalls {
				tcNames[tc.ID] = tc.Name
			}
			for _, r := range toolResults {
				s.stream(workflow.StreamEvent{
					Type:   "tool_result",
					NodeID: s.eventID,
					Payload: map[string]any{
						"tool_call_id": r.ToolCallID,
						"name":         tcNames[r.ToolCallID],
						"content":      r.Content,
						"is_error":     r.IsError,
					},
				})
			}
		}

		messages = append(messages, NewToolResultMessage(toolResults))
	}

	return &RoundResult{
		Content:     s.acc.String(),
		Message:     accMsg,
		Messages:    messages,
		ToolCalls:   toolCalls,
		ToolResults: toolResults,
		ToolPending: toolPending,
		Usage:       usage,
	}, nil
}

func buildRoundGenerateOptions(cfg RoundConfig, reg *tool.Registry) []GenerateOption {
	var opts []GenerateOption
	if cfg.Temperature != nil {
		opts = append(opts, WithTemperature(*cfg.Temperature))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Thinking {
		opts = append(opts, WithThinking(true))
	}
	if cfg.JSONMode {
		opts = append(opts, WithJSONMode(true))
	}

	var toolDefs []ToolDefinition
	if reg != nil && len(cfg.ToolNames) > 0 {
		allowed := make(map[string]bool, len(cfg.ToolNames))
		for _, name := range cfg.ToolNames {
			allowed[name] = true
		}
		for _, def := range reg.Definitions() {
			if allowed[def.Name] {
				toolDefs = append(toolDefs, def)
			}
		}
	}
	if len(toolDefs) > 0 {
		opts = append(opts, WithTools(toolDefs...))
	}
	return opts
}
