package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// CallRecorder observes the tool calls one run makes. It exists as a contract so
// that middleware which needs to record calls depends on this package rather than
// on the tool engine: the engine's session implements it, and a host that runs a
// bare [Executor] can supply its own.
type CallRecorder interface {
	// RecordCall notes that the model called call.
	RecordCall(call message.ToolCall)
}

type callRecorderKey struct{}

// WithCallRecorder attaches a recorder to ctx. A nil recorder returns ctx
// unchanged, so a caller that may not have one does not have to branch.
func WithCallRecorder(ctx context.Context, recorder CallRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, callRecorderKey{}, recorder)
}

// CallRecorderFrom returns the recorder attached to ctx, if any.
func CallRecorderFrom(ctx context.Context) (CallRecorder, bool) {
	recorder, ok := ctx.Value(callRecorderKey{}).(CallRecorder)
	return recorder, ok
}
