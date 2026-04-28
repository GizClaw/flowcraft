package agent

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

// Observer is a read-only lifecycle hook that lets callers react to
// stages of a [Run] without affecting its outcome. It is the plumbing
// behind agent's "history append on completion", "metric emit on
// start", "transcript snapshot on interrupt", and similar patterns,
// none of which agent hard-codes any more.
//
// Design rules:
//
//  1. Observers MUST NOT change the [Result] returned by Run. agent
//     intentionally exposes the Result to OnRunEnd by pointer because
//     it is the same value the caller will receive — observers may
//     stash references to it (for logging, async append, …) but
//     mutating it leaves agent's caller staring at the mutation. Treat
//     this surface as advisory.
//
//  2. Observer methods MUST NOT return an error. Failures inside an
//     observer are the observer's problem; they MUST NOT propagate
//     into Run. When an observer needs to fail or alter a turn (guard
//     hooks, moderation, disposition), use a [Decider] instead — its
//     explicit decision semantics keep the flow auditable.
//
//  3. Observer methods are called synchronously from Run on the
//     caller's goroutine. Blocking inside them blocks the run.
//     Long-running side effects MUST be dispatched asynchronously by
//     the observer itself.
//
//  4. Run guarantees the call sequence: OnRunStart fires exactly
//     once before engine.Execute; OnInterrupt fires at most once and
//     ONLY when the engine returned an [engine.InterruptedError]
//     (foreign-shape errors that merely satisfy errdefs.IsInterrupted
//     still classify the run as interrupted but skip OnInterrupt);
//     OnRunEnd fires exactly once after engine.Execute returns,
//     regardless of outcome.
//
// Embed [BaseObserver] to satisfy the interface with no-op defaults
// when only a subset of the methods are interesting.
type Observer interface {
	// OnRunStart fires after Run prepared the engine inputs but
	// before engine.Execute is invoked. info carries the immutable
	// identification fields agreed for this turn.
	OnRunStart(ctx context.Context, info RunInfo, req *Request)

	// OnInterrupt fires only when the engine returned an interrupt
	// error. It runs before OnRunEnd. intr carries the structured
	// reason supplied by the host.
	OnInterrupt(ctx context.Context, info RunInfo, intr engine.Interrupt)

	// OnRunEnd fires after engine.Execute returned and Run finished
	// classifying the outcome. res is the same pointer Run is about
	// to return; observers MUST treat it as read-only.
	OnRunEnd(ctx context.Context, info RunInfo, res *Result)
}

// BaseObserver provides no-op default implementations of every
// Observer method. Embed it in custom observers that only care about a
// subset of the lifecycle:
//
//	type historyAppender struct {
//	    agent.BaseObserver
//	    store sdk_history.History
//	}
//
//	func (h *historyAppender) OnRunEnd(ctx context.Context, info agent.RunInfo, res *agent.Result) {
//	    if res.Status != agent.StatusCompleted { return }
//	    _ = h.store.Append(ctx, info.ContextID, res.Messages)
//	}
type BaseObserver struct{}

// OnRunStart is a no-op.
func (BaseObserver) OnRunStart(context.Context, RunInfo, *Request) {}

// OnInterrupt is a no-op.
func (BaseObserver) OnInterrupt(context.Context, RunInfo, engine.Interrupt) {}

// OnRunEnd is a no-op.
func (BaseObserver) OnRunEnd(context.Context, RunInfo, *Result) {}

// Compile-time assertion BaseObserver satisfies Observer.
var _ Observer = BaseObserver{}

// RunInfo is the immutable identification bundle threaded through
// observer callbacks. It is small on purpose: anything beyond
// identification (board contents, request payload, result) is passed
// as a separate, typed argument so observers cannot accidentally
// hold onto a snapshot they aren't supposed to.
type RunInfo struct {
	// AgentID is the running [Agent.ID].
	AgentID string

	// RunID is the execution id assigned by Run (req.RunID when
	// supplied, else the auto-generated one).
	RunID string

	// TaskID echoes [Request.TaskID]. Empty when the caller did not
	// scope this turn to a long-running task.
	TaskID string

	// ContextID echoes [Request.ContextID]. Empty when the turn is
	// not part of a persistent conversation.
	ContextID string
}

// composeObservers returns a single Observer that fans every method
// out to obs in registration order, swallowing panics so one bad
// observer cannot tear down the run loop. nil entries are skipped.
//
// Returns nil when obs is empty so callers can branch on
// "no observers" without paying the dispatch cost.
func composeObservers(obs []Observer) Observer {
	filtered := obs[:0:0]
	for _, o := range obs {
		if o != nil {
			filtered = append(filtered, o)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return multiObserver(filtered)
}

type multiObserver []Observer

func (m multiObserver) OnRunStart(ctx context.Context, info RunInfo, req *Request) {
	for _, o := range m {
		safeRun(func() { o.OnRunStart(ctx, info, req) })
	}
}

func (m multiObserver) OnInterrupt(ctx context.Context, info RunInfo, intr engine.Interrupt) {
	for _, o := range m {
		safeRun(func() { o.OnInterrupt(ctx, info, intr) })
	}
}

func (m multiObserver) OnRunEnd(ctx context.Context, info RunInfo, res *Result) {
	for _, o := range m {
		safeRun(func() { o.OnRunEnd(ctx, info, res) })
	}
}

// safeRun invokes f, recovering from panics so a misbehaving observer
// cannot crash Run. The panic is intentionally dropped: observers are
// advisory, and there is no Run-level error channel to surface it on.
// In production we expect observability hooks to log internally before
// panicking.
func safeRun(f func()) {
	defer func() { _ = recover() }()
	f()
}
