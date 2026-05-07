package fakellm

import (
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// stream is a minimal llm.StreamMessage that splits the final
// message text into N-word chunks and emits a final FinishReason
// on the last chunk. Tool-call replies are emitted as a single
// chunk because tool deltas in vessel/v0.1.0 have no test value
// (no consumer relies on partial JSON arguments).
type stream struct {
	msg     llm.Message
	usage   llm.Usage
	chunks  []llm.StreamChunk
	idx     int
	current llm.StreamChunk
}

func newStream(msg llm.Message, usage llm.TokenUsage, words int) *stream {
	s := &stream{
		msg:   msg,
		usage: llm.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}
	s.build(words)
	return s
}

func (s *stream) build(words int) {
	if len(s.msg.ToolCalls()) > 0 {
		s.chunks = []llm.StreamChunk{{
			Role:         llm.RoleAssistant,
			ToolCalls:    s.msg.ToolCalls(),
			FinishReason: "tool_calls",
		}}
		return
	}
	text := s.msg.Content()
	if text == "" {
		s.chunks = []llm.StreamChunk{{Role: llm.RoleAssistant, FinishReason: "stop"}}
		return
	}
	parts := strings.Fields(text)
	if words <= 0 {
		words = 3
	}
	for i := 0; i < len(parts); i += words {
		end := i + words
		if end > len(parts) {
			end = len(parts)
		}
		chunk := strings.Join(parts[i:end], " ")
		if i > 0 {
			chunk = " " + chunk
		}
		s.chunks = append(s.chunks, llm.StreamChunk{Role: llm.RoleAssistant, Content: chunk})
	}
	s.chunks[len(s.chunks)-1].FinishReason = "stop"
}

func (s *stream) Next() bool {
	if s.idx >= len(s.chunks) {
		return false
	}
	s.current = s.chunks[s.idx]
	s.idx++
	return true
}

func (s *stream) Current() llm.StreamChunk { return s.current }
func (s *stream) Err() error               { return nil }
func (s *stream) Close() error             { return nil }
func (s *stream) Message() llm.Message     { return s.msg }
func (s *stream) Usage() llm.Usage         { return s.usage }
