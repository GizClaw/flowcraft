package inference

import "context"

// ToolCallDelta carries provider-neutral incremental tool-call arguments.
// ArgumentsFragment is validated only after stream accumulation.
type ToolCallDelta struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ArgumentsFragment string `json:"arguments_fragment,omitempty"`
}

type ProviderStream[RawEvent any] interface {
	Next(context.Context) (RawEvent, error)
	Close() error
}
