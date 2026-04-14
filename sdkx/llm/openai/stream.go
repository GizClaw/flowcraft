package openai

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type openaiStreamMessage struct {
	baseCtx context.Context
	span    trace.Span
	model   string
	start   time.Time
	stream  *ssestream.Stream[oai.ChatCompletionChunk]

	mu        sync.Mutex
	usage     llm.Usage
	content   strings.Builder
	toolCalls map[int]llm.ToolCall
	closeOnce sync.Once
	spanEnded bool

	cur llm.StreamChunk
	err error
}

func newStreamMessage(ctx context.Context, span trace.Span, model string, stream *ssestream.Stream[oai.ChatCompletionChunk]) llm.StreamMessage {
	return &openaiStreamMessage{
		baseCtx: ctx,
		span:    span,
		model:   model,
		start:   time.Now(),
		stream:  stream,
	}
}

func (s *openaiStreamMessage) Next() bool {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return false
	}
	if err := s.baseCtx.Err(); err != nil {
		s.err = err
		s.mu.Unlock()
		s.finish(err)
		return false
	}
	s.mu.Unlock()

	for {
		if !s.stream.Next() {
			err := s.stream.Err()
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}

		chunk := s.stream.Current()
		s.updateUsage(chunk)
		s.accumulateToolCalls(chunk)

		text := extractDeltaText(chunk)
		if text == "" {
			continue
		}

		s.mu.Lock()
		s.content.WriteString(text)
		s.cur = llm.StreamChunk{
			Role:    llm.RoleAssistant,
			Content: text,
		}
		if len(chunk.Choices) > 0 {
			s.cur.FinishReason = chunk.Choices[0].FinishReason
		}
		s.mu.Unlock()
		return true
	}
}

func (s *openaiStreamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *openaiStreamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *openaiStreamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *openaiStreamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		cerr = s.stream.Close()
		s.finish(cerr)
	})
	return cerr
}

// Message returns the fully accumulated assistant message after stream completes.
func (s *openaiStreamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []llm.Part
	if text := s.content.String(); text != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: text})
	}
	for _, tc := range s.sortedToolCalls() {
		tc := tc
		parts = append(parts, llm.Part{Type: llm.PartToolCall, ToolCall: &tc})
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func (s *openaiStreamMessage) accumulateToolCalls(chunk oai.ChatCompletionChunk) {
	if len(chunk.Choices) == 0 {
		return
	}
	for _, tc := range chunk.Choices[0].Delta.ToolCalls {
		idx := int(tc.Index)
		s.mu.Lock()
		if s.toolCalls == nil {
			s.toolCalls = make(map[int]llm.ToolCall)
		}
		existing := s.toolCalls[idx]
		if tc.ID != "" {
			existing.ID = tc.ID
		}
		if tc.Function.Name != "" {
			existing.Name += tc.Function.Name
		}
		existing.Arguments += tc.Function.Arguments
		s.toolCalls[idx] = existing
		s.mu.Unlock()
	}
}

func (s *openaiStreamMessage) sortedToolCalls() []llm.ToolCall {
	if len(s.toolCalls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(s.toolCalls))
	for idx := range s.toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	calls := make([]llm.ToolCall, 0, len(indices))
	for _, idx := range indices {
		calls = append(calls, s.toolCalls[idx])
	}
	return calls
}

func (s *openaiStreamMessage) updateUsage(chunk oai.ChatCompletionChunk) {
	if chunk.Usage.PromptTokens == 0 && chunk.Usage.CompletionTokens == 0 {
		return
	}
	s.mu.Lock()
	s.usage.InputTokens = chunk.Usage.PromptTokens
	s.usage.OutputTokens = chunk.Usage.CompletionTokens
	s.mu.Unlock()
}

func extractDeltaText(chunk oai.ChatCompletionChunk) string {
	if len(chunk.Choices) == 0 {
		return ""
	}
	d := chunk.Choices[0].Delta
	if d.Content != "" {
		return d.Content
	}
	if d.Refusal != "" {
		return d.Refusal
	}
	return ""
}

func (s *openaiStreamMessage) finish(err error) {
	s.mu.Lock()
	if s.spanEnded {
		s.mu.Unlock()
		return
	}
	s.spanEnded = true
	usage := s.usage
	s.mu.Unlock()

	dur := time.Since(s.start)

	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(s.baseCtx, "openai", s.model, "error", dur, llm.TokenUsage{})
	} else {
		s.span.SetAttributes(
			attribute.Int64("llm.input_tokens", usage.InputTokens),
			attribute.Int64("llm.output_tokens", usage.OutputTokens),
		)
		s.span.SetStatus(codes.Ok, "OK")
		llm.RecordLLMMetrics(s.baseCtx, "openai", s.model, "success", dur, llm.TokenUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.InputTokens + usage.OutputTokens,
		})
	}
	s.span.End()
}
