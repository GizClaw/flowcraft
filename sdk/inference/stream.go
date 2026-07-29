package inference

import "context"

// ToolCallDelta carries provider-neutral incremental tool-call arguments.
// ArgumentsFragment is validated only after stream accumulation.
type ToolCallDelta struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ArgumentsFragment string `json:"arguments_fragment,omitempty"`
}

// ReasoningDelta carries incremental reasoning text. Signature and ID are
// terminal-only: providers sign a reasoning block when it completes, so the
// last delta for a part carries the opaque verification payload and the
// provider-issued trace identifier. The accumulator concatenates Text and
// keeps the latest Signature and ID.
type ReasoningDelta struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
}

type ProviderStream[RawEvent any] interface {
	Next(context.Context) (RawEvent, error)
	Close() error
}
