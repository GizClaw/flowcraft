package responses

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	openaishared "github.com/GizClaw/flowcraft/sdkx/llm/openai/shared"

	"github.com/openai/openai-go/packages/ssestream"
	oairesponses "github.com/openai/openai-go/responses"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type streamMessage struct {
	baseCtx  context.Context
	span     trace.Span
	provider string
	model    string
	start    time.Time
	stream   *ssestream.Stream[oairesponses.ResponseStreamEventUnion]

	mu        sync.Mutex
	content   strings.Builder
	msg       llm.Message
	toolCalls map[int]llm.ToolCall
	usage     llm.Usage
	cur       llm.StreamChunk
	err       error
	closeOnce sync.Once
	spanEnded bool
}

func newStreamMessage(ctx context.Context, span trace.Span, provider, model string, stream *ssestream.Stream[oairesponses.ResponseStreamEventUnion]) llm.StreamMessage {
	return &streamMessage{
		baseCtx:  ctx,
		span:     span,
		provider: provider,
		model:    model,
		start:    time.Now(),
		stream:   stream,
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
		if !s.stream.Next() {
			err := s.stream.Err()
			if err != nil {
				if ctxErr := s.baseCtx.Err(); ctxErr != nil {
					err = errdefs.FromContext(fmt.Errorf("%s.responses.stream: %s: %w: %w", s.provider, time.Since(s.start).String(), err, ctxErr))
				} else {
					err = openaishared.ClassifyAPIErrorWithProvider(s.provider, err)
				}
			}
			s.mu.Lock()
			s.stream = nil
			s.err = err
			s.ensureMessageLocked()
			s.mu.Unlock()
			s.finish(err)
			return false
		}

		event := s.stream.Current()
		if err := s.applyEvent(event); err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}
		if text := eventDeltaText(event); text != "" {
			s.mu.Lock()
			s.content.WriteString(text)
			s.cur = llm.StreamChunk{Role: llm.RoleAssistant, Content: text}
			s.mu.Unlock()
			return true
		}
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
	s.ensureMessageLocked()
	return s.msg
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

func (s *streamMessage) applyEvent(event oairesponses.ResponseStreamEventUnion) error {
	switch event.Type {
	case "error":
		return classifyResponseError(s.provider, "stream error", event.Code, event.Message)
	case "response.failed":
		if event.Response.Error.Code != "" || event.Response.Error.Message != "" {
			return classifyResponseError(s.provider, "response failed", string(event.Response.Error.Code), event.Response.Error.Message)
		}
		return errdefs.NotAvailablef("%s: response failed", s.provider)
	case "response.incomplete":
		return classifyResponseIncomplete(s.provider, event.Response.IncompleteDetails.Reason)
	case "response.completed":
		msg := responseMessage(&event.Response)
		usage := responseUsage(&event.Response)
		s.mu.Lock()
		s.msg = msg
		s.usage.InputTokens = usage.InputTokens
		s.usage.CachedInputTokens = usage.CachedInputTokens
		s.usage.OutputTokens = usage.OutputTokens
		s.mu.Unlock()
	case "response.output_item.added", "response.output_item.done":
		s.accumulateOutputItem(event.OutputIndex, event.Item)
	case "response.function_call_arguments.delta":
		s.accumulateFunctionArguments(event.OutputIndex, event.Delta.OfString, "")
	case "response.function_call_arguments.done":
		s.accumulateFunctionArguments(event.OutputIndex, "", event.Arguments)
	}
	return nil
}

func eventDeltaText(event oairesponses.ResponseStreamEventUnion) string {
	if event.Type == "response.output_text.delta" {
		return event.Delta.OfString
	}
	return ""
}

func (s *streamMessage) accumulateOutputItem(outputIndex int64, item oairesponses.ResponseOutputItemUnion) {
	if item.Type != "function_call" {
		return
	}
	idx := int(outputIndex)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolCalls == nil {
		s.toolCalls = make(map[int]llm.ToolCall)
	}
	existing := s.toolCalls[idx]
	if item.CallID != "" {
		existing.ID = item.CallID
	}
	if item.Name != "" {
		existing.Name = item.Name
	}
	if item.Arguments != "" {
		existing.Arguments = item.Arguments
	}
	s.toolCalls[idx] = existing
}

func (s *streamMessage) accumulateFunctionArguments(outputIndex int64, delta, arguments string) {
	if delta == "" && arguments == "" {
		return
	}
	idx := int(outputIndex)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolCalls == nil {
		s.toolCalls = make(map[int]llm.ToolCall)
	}
	existing := s.toolCalls[idx]
	if arguments != "" {
		existing.Arguments = arguments
	} else {
		existing.Arguments += delta
	}
	s.toolCalls[idx] = existing
}

func (s *streamMessage) ensureMessageLocked() {
	if len(s.msg.Parts) == 0 {
		if text := s.content.String(); text != "" {
			s.msg.Parts = append(s.msg.Parts, llm.Part{Type: llm.PartText, Text: text})
		}
	}
	if !s.msg.HasToolCalls() {
		for _, tc := range s.sortedToolCallsLocked() {
			tc := tc
			s.msg.Parts = append(s.msg.Parts, llm.Part{Type: llm.PartToolCall, ToolCall: &tc})
		}
	}
	if len(s.msg.Parts) > 0 {
		s.msg.Role = llm.RoleAssistant
	}
}

func (s *streamMessage) sortedToolCallsLocked() []llm.ToolCall {
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

func (s *streamMessage) finish(err error) {
	s.mu.Lock()
	if s.spanEnded {
		s.mu.Unlock()
		return
	}
	s.spanEnded = true
	usage := s.usage
	s.mu.Unlock()

	dur := time.Since(s.start)
	tokens := llm.TokenUsage{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.InputTokens + usage.OutputTokens,
	}
	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "error", dur, tokens)
	} else {
		s.span.SetAttributes(llm.UsageSpanAttrs(tokens)...)
		s.span.SetStatus(codes.Ok, "OK")
		llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "success", dur, tokens)
	}
	s.span.End()
}
