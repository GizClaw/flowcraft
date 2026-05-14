package llm

import "github.com/GizClaw/flowcraft/sdk/model"

// NewOneChunkStream wraps a completed Generate result as a
// [StreamMessage] that yields exactly one chunk and then ends. It is
// the canonical adapter helper for synchronous-only providers (image
// generation, audio generation, providers without native streaming)
// that still need to satisfy the [LLM.GenerateStream] contract.
//
// It is also the downgrade target used by [WithCaps] when CapStreaming
// is disabled — see (*capsLLM).GenerateStream.
//
// Semantics:
//   - First Next() returns true; Current() yields a [model.StreamChunk]
//     synthesised from the message (Role + concatenated text +
//     ToolCalls + FinishReason="stop"). Non-text Parts (image / audio /
//     file) are NOT replayed in the chunk because [model.StreamChunk]
//     is text-oriented; callers needing the full multimodal payload
//     read [StreamMessage.Message] after iteration completes.
//   - Second Next() returns false. Err() returns nil.
//   - Close() is a no-op and idempotent.
//   - Message() returns the original message; Usage() returns the
//     captured usage.
func NewOneChunkStream(msg Message, usage TokenUsage) StreamMessage {
	return &oneChunkStream{
		msg: msg,
		usage: model.Usage{
			InputTokens:       usage.InputTokens,
			CachedInputTokens: usage.CachedInputTokens,
			OutputTokens:      usage.OutputTokens,
		},
	}
}

type oneChunkStream struct {
	msg     Message
	usage   model.Usage
	emitted bool
	cur     model.StreamChunk
}

func (s *oneChunkStream) Next() bool {
	if s.emitted {
		return false
	}
	s.cur = model.StreamChunk{
		Role:         s.msg.Role,
		Content:      s.msg.Content(),
		ToolCalls:    s.msg.ToolCalls(),
		FinishReason: "stop",
	}
	s.emitted = true
	return true
}

func (s *oneChunkStream) Current() model.StreamChunk { return s.cur }
func (s *oneChunkStream) Err() error                 { return nil }
func (s *oneChunkStream) Close() error               { return nil }
func (s *oneChunkStream) Message() Message           { return s.msg }
func (s *oneChunkStream) Usage() model.Usage         { return s.usage }
