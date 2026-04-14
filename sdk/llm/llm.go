package llm

import "context"

// LLM is the unified interface for language model interactions.
type LLM interface {
	Generate(ctx context.Context, messages []Message, opts ...GenerateOption) (Message, TokenUsage, error)
	GenerateStream(ctx context.Context, messages []Message, opts ...GenerateOption) (StreamMessage, error)
}

// StreamMessage is an iterator over streaming response chunks.
// Call Next() in a loop; Current() returns the latest chunk.
// After iteration completes, Message() and Usage() return the
// accumulated result.
type StreamMessage interface {
	Next() bool
	Current() StreamChunk
	Err() error
	Close() error
	Message() Message
	Usage() Usage
}
