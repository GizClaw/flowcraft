package tooltest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// RecordedCall is one observed tool invocation.
type RecordedCall struct {
	Arguments string
}

// RecordingTool is a tool.Tool that records every invocation and returns a
// configured response or error.
type RecordingTool struct {
	name string

	mu       sync.Mutex
	calls    []RecordedCall
	response string
	err      error
}

// NewRecordingTool returns a recording tool with an empty-object schema.
func NewRecordingTool(name string) *RecordingTool {
	return &RecordingTool{name: name}
}

// SetResponse configures subsequent Execute results.
func (r *RecordingTool) SetResponse(response string, err error) {
	r.mu.Lock()
	r.response = response
	r.err = err
	r.mu.Unlock()
}

// Calls returns a copy of every observed invocation.
func (r *RecordingTool) Calls() []RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// Definition implements tool.Tool.
func (r *RecordingTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{
		Name:        r.name,
		InputSchema: []byte(`{"type":"object"}`),
	}
}

// Execute implements tool.Tool.
func (r *RecordingTool) Execute(_ context.Context, arguments string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, RecordedCall{Arguments: arguments})
	response, err := r.response, r.err
	r.mu.Unlock()
	return response, err
}

var _ tool.Tool = (*RecordingTool)(nil)
