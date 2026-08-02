package agent

import "context"

// ---------- Observer (read-only lifecycle) ----------

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
//     hooks, moderation, disposition), use a [Referee] instead — its
//     explicit decision semantics keep the flow auditable.
//
//  3. Observer methods are called synchronously from Run on the
//     caller's goroutine. Blocking inside them blocks the run.
//     Long-running side effects MUST be dispatched asynchronously by
//     the observer itself.
//
//  4. Run guarantees the call sequence: OnRunStart fires exactly
//     once before Execute; OnInterrupt fires at most once and
//     ONLY when the engine returned an [InterruptedError]
//     (foreign-shape errors that merely satisfy errdefs.IsInterrupted
//     still classify the run as interrupted but skip OnInterrupt);
//     OnRunEnd fires exactly once after Execute returns,
//     regardless of outcome.
//
// Embed [BaseObserver] to satisfy the interface with no-op defaults
// when only a subset of the lifecycle is interesting:
//
//	type historyAppender struct {
//	    agent.BaseObserver
//	    store sdk_history.History
//	}
//
//	func (h *historyAppender) OnRunEnd(ctx context.Context, id agent.Identity, res *agent.Result) {
//	    if res.Status != agent.StatusCompleted { return }
//	    _ = h.store.Append(ctx, id.ContextID, res.Messages)
//	}
type Observer interface {
	// OnRunStart fires after Run prepared the engine inputs but
	// before Execute is invoked. id carries the immutable
	// identification fields agreed for this turn.
	OnRunStart(ctx context.Context, id Identity, req *Request)

	// OnInterrupt fires only when the engine returned an interrupt
	// error. It runs before OnRunEnd. intr carries the structured
	// reason supplied by the host.
	OnInterrupt(ctx context.Context, id Identity, intr Interrupt)

	// OnRunRevise fires when a Referee asked agent.Execute to re-invoke
	// Execute (Decision{Revise: true}) AND the
	// per-call WithMaxRevise budget allows another attempt. It
	// runs after the discarded attempt's classification but BEFORE
	// the next OnRunStart, so observers see the lifecycle as:
	//
	//	OnRunStart → Execute → OnRunRevise → OnRunStart → Execute → OnRunEnd
	//
	// prevRes is the (about-to-be-replaced) Result from the failed
	// attempt — observers MUST treat it as read-only. nextAttempt
	// is the 1-indexed attempt number the next Execute will
	// be (== prevRes.Attempts + 1).
	//
	// OnRunRevise fires zero times for runs that complete on the first
	// attempt or whose Referee never asks for revise.
	OnRunRevise(ctx context.Context, id Identity, prevRes *Result, nextAttempt int)

	// OnRunEnd fires after Execute returned and Run finished
	// classifying the outcome. res is the same pointer Run is about
	// to return; observers MUST treat it as read-only.
	OnRunEnd(ctx context.Context, id Identity, res *Result)
}

// BaseObserver provides no-op default implementations of every
// Observer method. Embed it in custom observers that only care
// about a subset of the lifecycle:
//
//	type historyAppender struct {
//	    agent.BaseObserver
//	    store sdk_history.History
//	}
//
//	func (h *historyAppender) OnRunEnd(ctx context.Context, id agent.Identity, res *agent.Result) {
//	    if res.Status != agent.StatusCompleted { return }
//	    _ = h.store.Append(ctx, id.ContextID, res.Messages)
//	}
type BaseObserver struct{}

// OnRunStart is a no-op.
func (BaseObserver) OnRunStart(context.Context, Identity, *Request) {}

// OnInterrupt is a no-op.
func (BaseObserver) OnInterrupt(context.Context, Identity, Interrupt) {}

// OnRunRevise is a no-op.
func (BaseObserver) OnRunRevise(context.Context, Identity, *Result, int) {}

// OnRunEnd is a no-op.
func (BaseObserver) OnRunEnd(context.Context, Identity, *Result) {}

// Compile-time assertion BaseObserver satisfies Observer.
var _ Observer = BaseObserver{}

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

func (m multiObserver) OnRunStart(ctx context.Context, id Identity, req *Request) {
	for _, o := range m {
		safeRun(func() { o.OnRunStart(ctx, id, req) })
	}
}

func (m multiObserver) OnInterrupt(ctx context.Context, id Identity, intr Interrupt) {
	for _, o := range m {
		safeRun(func() { o.OnInterrupt(ctx, id, intr) })
	}
}

func (m multiObserver) OnRunRevise(ctx context.Context, id Identity, prev *Result, next int) {
	for _, o := range m {
		safeRun(func() { o.OnRunRevise(ctx, id, prev, next) })
	}
}

func (m multiObserver) OnRunEnd(ctx context.Context, id Identity, res *Result) {
	for _, o := range m {
		safeRun(func() { o.OnRunEnd(ctx, id, res) })
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

// ---------- Preparer (board construction) ----------

// Preparer builds the initial [Board] for a run, building on the
// board left by the previous link in the chain.
//
// A chain of Preparers is a linear pipeline: agent runs them in
// registration order, threading the board through them. The first
// link in the chain receives a board freshly seeded with
// req.Message on MainChannel and req.Inputs as board vars; each
// subsequent link receives the board its predecessor returned. Every
// Preparer must return a fresh *Board (do not mutate and re-yield
// the input) so the engine and downstream chain links can rely on
// immutability of the previous board.
//
// It is the single extension point for "anything that should be on
// the board before the engine sees it":
//
//   - conversation history (load from memory/history, summarise, window);
//   - retrieved long-term memory (memory/recall results, knowledge-base
//     hits);
//   - system prompts and persona text;
//   - request-scoped board vars (form fields, parameters, tool
//     allow-lists);
//   - any combination of the above.
//
// Run guarantees:
//
//   - The chain is called exactly once per Run attempt, before
//     Execute and before any Observer's OnRunStart. Revise attempts
//     re-run the chain from the beginning with the same Request so
//     boards are not stale across retries.
//   - The returned board is mutated by the engine; Preparers MUST
//     return a fresh value each call.
//   - The returned board MUST be non-nil. Returning nil is a Run
//     infrastructure error.
//
// Implementations are expected to be cheap and synchronous; long
// async work (retrieval, IO) belongs in a wrapper that resolves
// before Run.
type Preparer interface {
	Before(ctx context.Context, id Identity, req *Request, prev *Board) (*Board, error)
}

// PreparerFunc is the function-typed adapter for Preparer.
//
// Useful when the seed logic is a single closure over a transcript
// loader or retriever:
//
//	agent.WithPreparer(agent.PreparerFunc(func(ctx context.Context, id agent.Identity, req *agent.Request, prev *agent.Board) (*agent.Board, error) {
//	    b := prev.Clone()
//	    b.SetChannel("memory", retrieved)
//	    return b, nil
//	}))
type PreparerFunc func(ctx context.Context, id Identity, req *Request, prev *Board) (*Board, error)

// Before calls f.
func (f PreparerFunc) Before(ctx context.Context, id Identity, req *Request, prev *Board) (*Board, error) {
	return f(ctx, id, req, prev)
}

// seedBoard runs the Preparer chain against a fresh default board.
// The default seed appends req.Message to MainChannel and copies
// req.Inputs into board vars; chain links that want a different
// starting state can ignore prev and return their own. A nil or
// empty chain returns the default board unchanged.
func seedBoard(ctx context.Context, id Identity, req *Request, chain []Preparer) (*Board, error) {
	board := NewBoard()
	board.AppendChannelMessage(MainChannel, req.Message)
	for k, v := range req.Inputs {
		board.SetVar(k, v)
	}
	for _, p := range chain {
		next, err := p.Before(ctx, id, req, board)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, nil // caller will translate to a validation error
		}
		board = next
	}
	return board, nil
}

// ---------- Referee (decision) ----------

// Referee is a decision-making lifecycle hook that can influence what
// agent.Execute does at well-defined boundaries. It is the read-write
// counterpart of [Observer]:
//
//   - Observers see what happened and emit side effects (logs,
//     metrics, transcript persistence).
//   - Referees return a structured decision agent.Run interprets.
//
// Referees expose one decision point — [Referee.After] — which fires
// after Execute returned but before [Run] commits the produced
// messages to history (i.e., before any Observer's OnRunEnd). This
// covers two real cases:
//
//  1. Disposition: a barge-in cause means the assistant was cut off
//     mid-thought; the half-baked output should not appear in the
//     persistent transcript. A Referee returns
//     Decision{DiscardOutput: true}.
//
//  2. (Reserved) Revise: the natural answer fails some quality bar
//     (no citations, policy violation, refusal-without-reason); the
//     Referee asks for one more model pass. The wire field is
//     present for forward compatibility — agent does not yet honour
//     it, and engines will need explicit support before it has any
//     effect.
//
// # Composition
//
// Multiple Referees may be registered (Agent-scoped + per-call).
// They run in registration order. The merged decision is the OR over
// boolean fields: any Referee asking to discard wins; same for
// revise. The first non-empty Reason wins, so callers can attribute
// the decision in logs.
//
// # Error contract
//
// A Referee returning a non-nil error short-circuits the merge and
// causes Run to return (Result, decider-error). agent does NOT swap
// the error class — it surfaces the Referee's own error so callers
// can classify with errdefs. The Result is still populated
// (including the engine's output) so the caller can decide what to
// do next.
//
// Embed [BaseReferee] to satisfy the interface with no-op defaults.
type Referee interface {
	// After fires after Execute returns. The Referee
	// inspects res (read-only) and the original req, and returns a
	// Decision that Run merges with other Referees' decisions.
	//
	// id carries the immutable identification fields agreed for
	// this turn. The Referee MUST NOT mutate res; agent will surface
	// the merged decision via [Result.Committed] and (when a Reason
	// was supplied) [Result.State]["finalize_reason"].
	After(ctx context.Context, id Identity, req *Request, res *Result) (Decision, error)
}

// Decision is the return type of [Referee.After]. The
// zero value means "no opinion" — agent applies its defaults.
//
// Defaults agent.Run uses when no Referee returns a directive:
//
//   - StatusCompleted runs are committed.
//   - StatusInterrupted / StatusCanceled / StatusAborted /
//     StatusFailed runs are NOT committed (their partial output is
//     dropped from the transcript view). This matches the
//     conservative behaviour Round A had hard-coded; round B simply
//     makes it overridable.
type Decision struct {
	// DiscardOutput, when true, instructs Run to mark Result.Committed
	// = false regardless of Status. Observers reading Committed
	// (notably history-append observers) skip persistence on a
	// discarded run.
	//
	// Setting DiscardOutput on a StatusCompleted run is allowed and
	// useful for moderation hooks ("the answer violates policy, do
	// not persist it").
	DiscardOutput bool

	// Revise asks agent.Execute to discard this attempt's output and
	// re-invoke Execute with a fresh board (re-seeded from
	// the original Request). Honoured ONLY when the per-call
	// [WithMaxRevise] budget allows another attempt; the option
	// defaults to 0, so by default Revise is recorded as a
	// finalize_reason but does NOT trigger another engine call —
	// callers must opt in explicitly to avoid runaway loops on
	// faulty Referees.
	//
	// When honoured the lifecycle is:
	//
	//   1. Referee returns Revise=true (and optionally Reason).
	//   2. Run fires [Observer.OnRunRevise] with the about-to-be-
	//      discarded Result and the next attempt index.
	//   3. Board is re-seeded via the Preparer chain; the
	//      same Run identifier is reused so observers that
	//      key by run id can correlate attempts.
	//   4. Execute runs again. The Referee chain runs again
	//      on the new Result.
	//   5. The loop exits when either Revise=false or the attempt
	//      counter reaches WithMaxRevise. The final Result.Attempts
	//      reflects how many Execute calls were made.
	//
	// The Reason field is exposed via [Result.State]["finalize_reason"]
	// regardless of whether the Revise was honoured, so callers
	// can audit why a particular attempt did not commit.
	Revise bool

	// Reason, when non-empty, is propagated into
	// [Result.State]["finalize_reason"] and is also returned to the
	// caller through the same field. Referees that want to attribute
	// a decision in logs (e.g. "moderation:violation") set this
	// rather than relying on log scraping.
	//
	// Composition rule: the first non-empty Reason wins. This means
	// Referees later in the chain should only set Reason if they
	// have something more specific to add.
	Reason string
}

// BaseReferee provides no-op default implementations of every Referee
// method. Embed it in custom referees that only override After.
type BaseReferee struct{}

// After is a no-op that returns a zero Decision (no opinion).
func (BaseReferee) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return Decision{}, nil
}

// Compile-time assertion BaseReferee satisfies Referee.
var _ Referee = BaseReferee{}

// composeReferees merges the decisions of every Referee in
// registration order. Boolean fields are OR-merged; the first
// non-empty Reason wins. The first non-nil error short-circuits and
// is returned to the caller, with the partial Decision discarded.
//
// Returns the zero Decision and a nil error when refs is empty.
func composeReferees(ctx context.Context, id Identity, req *Request, res *Result, refs []Referee) (Decision, error) {
	var merged Decision
	for _, r := range refs {
		if r == nil {
			continue
		}
		d, err := r.After(ctx, id, req, res)
		if err != nil {
			return Decision{}, err
		}
		if d.DiscardOutput {
			merged.DiscardOutput = true
		}
		if d.Revise {
			merged.Revise = true
		}
		if merged.Reason == "" && d.Reason != "" {
			merged.Reason = d.Reason
		}
	}
	return merged, nil
}

// DiscardOnInterruptCauses is a Referee factory: when ANY of the
// named causes fires, DiscardOutput is set so a barge-in (or
// equivalent host-side abort) keeps partial assistant output out
// of the persistent transcript.
//
// Construct it with [NewDiscardOnInterruptCauses] and register as
// one of [Agent.Referees] or via [WithReferee].
type DiscardOnInterruptCauses struct {
	Reason string
	causes map[Cause]struct{}
}

// NewDiscardOnInterruptCauses builds a Referee that marks a run
// discarded whenever its interrupt cause matches one of causes.
// Reason is the string used as [Decision.Reason] when discarding
// fires, and is also surfaced in [Result.State]["finalize_reason"].
func NewDiscardOnInterruptCauses(reason string, causes ...Cause) *DiscardOnInterruptCauses {
	set := make(map[Cause]struct{}, len(causes))
	for _, c := range causes {
		set[c] = struct{}{}
	}
	return &DiscardOnInterruptCauses{Reason: reason, causes: set}
}

// After implements [Referee]. It inspects res.Status and the
// interrupt cause attached to the run, returning a discarding
// Decision when both are present and the cause matches.
func (d *DiscardOnInterruptCauses) After(_ context.Context, _ Identity, _ *Request, res *Result) (Decision, error) {
	if res == nil || res.Status != StatusInterrupted {
		return Decision{}, nil
	}
	if res.Cause == "" {
		return Decision{}, nil
	}
	if _, ok := d.causes[res.Cause]; !ok {
		return Decision{}, nil
	}
	return Decision{DiscardOutput: true, Reason: d.Reason}, nil
}
