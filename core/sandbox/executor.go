package sandbox

import "context"

// Executor is the optional one-shot capability of a [Runner].
//
// The one-shot form is otherwise derived: engine.Exec drains a [Session] from
// [Runner.Start] and truncates its output, so a backend has a single code path.
// That derivation cannot see the call as a whole, which is exactly what some
// decorators need: escalation recognizes an OS refusal in the completed result
// and re-runs the command outside the confine, while a Start-level wrapper only
// ever sees a live session.
//
// A runner that implements Executor takes over the whole one-shot contract for
// engine.Exec, not just the command:
//
//   - honour ExecOptions.Timeout and the caller's ctx;
//   - truncate each stream per ExecOptions.Resources.MaxOutputBytes;
//   - report a non-zero exit in [ExecResult] as a result, never as an error;
//   - own every session it starts, so nothing outlives the call.
//
// The supported implementation delegates to engine.Exec on an inner runner
// ([engine.Forwarder] forwards everything else), which keeps the derivation
// intact one layer down. A runner that does not implement this keeps the
// derived view over Start — backends need not implement it at all, and callers
// never assert it themselves: engine.Exec is the single place that knows it.
type Executor interface {
	Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
}
