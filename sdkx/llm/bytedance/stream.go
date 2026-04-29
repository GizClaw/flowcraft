package bytedance

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type arkStream interface {
	Recv() (model.ChatCompletionStreamResponse, error)
	Close() error
}

type streamMessage struct {
	baseCtx context.Context
	span    trace.Span
	model   string
	stream  arkStream

	mu        sync.Mutex
	usage     llm.Usage
	content   string
	toolCalls map[int]llm.ToolCall
	closeOnce sync.Once
	spanEnded bool

	cur llm.StreamChunk
	err error
}

func newStreamMessage(ctx context.Context, span trace.Span, modelName string, stream arkStream) llm.StreamMessage {
	return &streamMessage{
		baseCtx: ctx,
		span:    span,
		model:   modelName,
		stream:  stream,
	}
}

func (s *streamMessage) Next() bool {
	s.mu.Lock()
	if s.err != nil || s.stream == nil {
		s.mu.Unlock()
		return false
	}
	if err := s.baseCtx.Err(); err != nil {
		s.err = errdefs.FromContext(err)
		s.mu.Unlock()
		s.finish(s.err)
		return false
	}
	s.mu.Unlock()

	for {
		resp, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.mu.Lock()
				s.stream = nil
				s.mu.Unlock()
				s.finish(nil)
				return false
			}
			err = errdefs.ClassifyProviderError("bytedance", err)
			s.mu.Lock()
			s.stream = nil
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}

		s.updateUsage(resp)
		s.accumulateToolCalls(resp)

		text := extractDeltaText(resp)
		if text == "" {
			continue
		}

		s.mu.Lock()
		s.content += text
		s.cur = llm.StreamChunk{
			Role:         llm.RoleAssistant,
			Content:      text,
			FinishReason: extractFinishReason(resp),
		}
		s.mu.Unlock()
		return true
	}
}

func (s *streamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *streamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *streamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *streamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []llm.Part
	if s.content != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: s.content})
	}
	for _, tc := range s.sortedToolCalls() {
		tc := tc
		parts = append(parts, llm.Part{Type: llm.PartToolCall, ToolCall: &tc})
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func (s *streamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		if s.stream != nil {
			cerr = s.stream.Close()
			s.stream = nil
		}
		s.finish(cerr)
	})
	return cerr
}

func (s *streamMessage) accumulateToolCalls(resp model.ChatCompletionStreamResponse) {
	if len(resp.Choices) == 0 || resp.Choices[0] == nil {
		return
	}
	for _, tc := range resp.Choices[0].Delta.ToolCalls {
		if tc == nil {
			continue
		}
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
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

func (s *streamMessage) sortedToolCalls() []llm.ToolCall {
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

func (s *streamMessage) updateUsage(resp model.ChatCompletionStreamResponse) {
	if resp.Usage == nil {
		return
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return
	}
	s.mu.Lock()
	s.usage.InputTokens = int64(resp.Usage.PromptTokens)
	s.usage.OutputTokens = int64(resp.Usage.CompletionTokens)
	s.mu.Unlock()
}

func (s *streamMessage) finish(err error) {
	s.mu.Lock()
	if s.spanEnded {
		s.mu.Unlock()
		return
	}
	s.spanEnded = true
	usage := s.usage
	s.mu.Unlock()

	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
	} else {
		s.span.SetAttributes(
			attribute.Int64("llm.input_tokens", usage.InputTokens),
			attribute.Int64("llm.output_tokens", usage.OutputTokens),
		)
		s.span.SetStatus(codes.Ok, "OK")
	}
	s.span.End()
}
