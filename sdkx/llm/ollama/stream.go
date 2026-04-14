package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/llm"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ollamaStreamMessage struct {
	baseCtx context.Context
	span    trace.Span
	model   string

	mu       sync.Mutex
	dec      *json.Decoder
	body     io.ReadCloser
	doneSeen bool

	usage     llm.Usage
	content   strings.Builder
	toolCalls []llm.ToolCall
	spanEnded bool
	closeOnce sync.Once

	cur llm.StreamChunk
	err error
}

func newStreamMessage(
	ctx context.Context,
	span trace.Span,
	model string,
	body io.ReadCloser,
) llm.StreamMessage {
	return &ollamaStreamMessage{
		baseCtx: ctx,
		span:    span,
		model:   model,
		dec:     json.NewDecoder(body),
		body:    body,
	}
}

func (s *ollamaStreamMessage) Next() bool {
	s.mu.Lock()
	if s.body == nil || s.err != nil {
		s.mu.Unlock()
		return false
	}
	if s.doneSeen {
		body := s.body
		s.body = nil
		s.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		s.finish(nil)
		return false
	}
	if err := s.baseCtx.Err(); err != nil {
		body := s.body
		s.body = nil
		s.err = err
		s.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		s.finish(err)
		return false
	}
	dec := s.dec
	s.mu.Unlock()

	for {
		var chunk chatResponse
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				s.mu.Lock()
				body := s.body
				s.body = nil
				s.mu.Unlock()
				if body != nil {
					_ = body.Close()
				}
				s.finish(nil)
				return false
			}
			s.mu.Lock()
			body := s.body
			s.body = nil
			s.err = err
			s.mu.Unlock()
			if body != nil {
				_ = body.Close()
			}
			s.finish(err)
			return false
		}

		s.updateFromChunk(chunk)
		s.accumulateToolCalls(chunk)
		text := chunk.Message.Content

		if chunk.Done {
			if text != "" {
				s.mu.Lock()
				s.content.WriteString(text)
				s.cur = llm.StreamChunk{Role: llm.RoleAssistant, Content: text}
				s.doneSeen = true
				s.mu.Unlock()
				return true
			}
			s.mu.Lock()
			body := s.body
			s.body = nil
			s.doneSeen = true
			s.mu.Unlock()
			if body != nil {
				_ = body.Close()
			}
			s.finish(nil)
			return false
		}

		if text == "" && len(chunk.Message.ToolCalls) == 0 {
			continue
		}

		s.mu.Lock()
		if text != "" {
			s.content.WriteString(text)
		}
		s.cur = llm.StreamChunk{Role: llm.RoleAssistant, Content: text}
		s.mu.Unlock()
		return true
	}
}

func (s *ollamaStreamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *ollamaStreamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *ollamaStreamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *ollamaStreamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		body := s.body
		s.body = nil
		s.mu.Unlock()
		if body != nil {
			cerr = body.Close()
		}
		s.finish(cerr)
	})
	return cerr
}

func (s *ollamaStreamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []llm.Part
	if text := s.content.String(); text != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: text})
	}
	for i := range s.toolCalls {
		parts = append(parts, llm.Part{Type: llm.PartToolCall, ToolCall: &s.toolCalls[i]})
	}
	if len(parts) == 0 {
		return llm.Message{Role: llm.RoleAssistant}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func (s *ollamaStreamMessage) accumulateToolCalls(chunk chatResponse) {
	if len(chunk.Message.ToolCalls) == 0 {
		return
	}
	s.mu.Lock()
	for _, tc := range chunk.Message.ToolCalls {
		argsBytes, _ := json.Marshal(tc.Function.Arguments)
		s.toolCalls = append(s.toolCalls, llm.ToolCall{
			Name:      tc.Function.Name,
			Arguments: string(argsBytes),
		})
	}
	s.mu.Unlock()
}

func (s *ollamaStreamMessage) updateFromChunk(chunk chatResponse) {
	if chunk.PromptEvalCount != 0 || chunk.EvalCount != 0 {
		s.mu.Lock()
		s.usage.InputTokens = chunk.PromptEvalCount
		s.usage.OutputTokens = chunk.EvalCount
		s.mu.Unlock()
	}
}

func (s *ollamaStreamMessage) finish(err error) {
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
