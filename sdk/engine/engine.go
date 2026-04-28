package engine

import "context"

// Engine is the deliberately thin contract every local execution
// engine satisfies so the agent layer can drive it through a uniform
// shape. Concrete engines (sdk/graph DAG executor, future script-
// based engines, …) usually expose richer APIs in addition to this
// method.
//
// Contract:
//
//   - Execute MUST run to completion, until interrupted, or until
//     ctx is cancelled.
//
//   - On clean completion, return the final Board (often the same
//     pointer as the input — engines mutate in place by default) and
//     a nil error.
//
//   - On cooperative interrupt (host sent through host.Interrupts()),
//     return the (partial) Board together with the result of
//     [Interrupted]. The error then satisfies
//     errdefs.IsInterrupted(err) and can be destructured into an
//     [InterruptedError] for the cause.
//
//   - On ctx cancellation, return the (partial) Board and ctx.Err().
//
//   - On any other failure, return the (partial) Board together with
//     a domain error (preferably classified via errdefs). Returning a
//     non-nil board on error lets the host decide whether to commit /
//     discard / persist.
//
//   - When run.ResumeFrom is non-nil, Execute resumes from that
//     checkpoint instead of starting fresh. See [Run.ResumeFrom] for
//     the resume contract; engines that do not support resume MUST
//     return an errdefs.NotAvailable-classified error rather than
//     silently restarting.
//
// Engines MUST NOT close any host-owned channel and MUST NOT mutate
// run.Attributes or run.ResumeFrom.
type Engine interface {
	Execute(ctx context.Context, run Run, host Host, board *Board) (*Board, error)
}

// EngineFunc adapts a plain function to the [Engine] interface.
// Useful for test doubles and trivial engines.
type EngineFunc func(ctx context.Context, run Run, host Host, board *Board) (*Board, error)

// Execute satisfies [Engine].
func (f EngineFunc) Execute(ctx context.Context, run Run, host Host, board *Board) (*Board, error) {
	if f == nil {
		return board, nil
	}
	return f(ctx, run, host, board)
}
