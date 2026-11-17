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

// WithCallRecorder attaches a recorder to ctx unless the context already carries
// the run's scope, so a recorder never shadows the one a run already attached. A
// nil recorder returns ctx unchanged, so a caller that may not have one does not
// have to branch.
//
// Deprecated: use [EnsureScope]; the recorder is the Recorder field of the run's
// [Scope].
func WithCallRecorder(ctx context.Context, recorder CallRecorder) context.Context {
	return EnsureScope(ctx, recorder)
}

// CallRecorderFrom returns the recorder held by the scope attached to ctx, if
// any.
//
// Deprecated: use [ScopeFrom]; the recorder is the Recorder field of the run's
// [Scope].
func CallRecorderFrom(ctx context.Context) (CallRecorder, bool) {
	scope, ok := ScopeFrom(ctx)
	if !ok {
		return nil, false
	}
	return scope.Recorder, true
}
