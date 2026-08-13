package agent

import (
	"context"
	"maps"
)

// ---------- Observer (read-only lifecycle) ----------

// Observer is a read-only lifecycle hook that lets callers react to
// stages of a [Run] without affecting its outcome. It is the plumbing
// behind agent's "metric emit on start", "transcript snapshot on
// interrupt", notifications, and similar best-effort patterns,
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
//     into Run. Use a [Committer] for durable side effects that must
//     report failure, or a [Referee] for policy decisions that alter
//     whether a turn is accepted.
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
//	type metricsObserver struct {
//	    agent.BaseObserver
//	    metrics Metrics
//	}
//
//	func (o *metricsObserver) OnRunEnd(ctx context.Context, id agent.Identity, res *agent.Result) {
//	    o.metrics.Record(res)
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
//	type metricsObserver struct {
//	    agent.BaseObserver
//	    metrics Metrics
//	}
//
//	func (o *metricsObserver) OnRunEnd(ctx context.Context, id agent.Identity, res *agent.Result) {
//	    o.metrics.Record(res)
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
//   - conversation history (load, summarise, window);
//   - retrieved long-term memory and knowledge-base hits;
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
// Preparers may perform bounded I/O such as transcript loading or
// retrieval. They run synchronously on the caller's goroutine, MUST
// honor ctx cancellation and deadlines, and SHOULD apply their own
// tighter timeout when calling a dependency whose latency is not
// already bounded. A Preparer MUST NOT detach untracked background
// work from the run.
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

// ---------- Commit view (Committer-only projection) ----------

// CommitView is the board projection a [CommitViewProvider] exposes only
// to [Committer] hooks. The engine-produced board remains authoritative
// for the Result returned to the caller and delivered to [Observer].
type CommitView struct {
	// LastBoard is materialized into the shallow Result copy passed to
	// Committers. It must be non-nil.
	LastBoard *Board
}

// CommitViewProvider builds a Committer-only projection after the final
// [Referee] decision accepts the result and immediately before the
// Committer chain runs.
//
// Providers run only when at least one Committer is registered and the
// final Result is committed. They MUST honor ctx cancellation and MUST
// NOT mutate req or res. Returning an error, or a CommitView with a nil
// LastBoard, skips every Committer and fails finalization.
type CommitViewProvider interface {
	CommitView(ctx context.Context, id Identity, req *Request, res *Result) (CommitView, error)
}

// CommitViewProviderFunc adapts a function to [CommitViewProvider].
type CommitViewProviderFunc func(ctx context.Context, id Identity, req *Request, res *Result) (CommitView, error)

// CommitView calls f.
func (f CommitViewProviderFunc) CommitView(
	ctx context.Context,
	id Identity,
	req *Request,
	res *Result,
) (CommitView, error) {
	return f(ctx, id, req, res)
}

// ---------- Committer (durable finalization) ----------

// Committer persists or durably enqueues the final accepted result of a
// run. It is the reliable side-effect boundary between [Referee] and
// [Observer]:
//
//   - Referees decide whether the final result is accepted.
//   - Committers make an accepted result durable and may fail the run.
//   - Observers react best-effort after commitment has succeeded or
//     failed.
//
// Committers run synchronously in registration order and only when the
// final [Result] has Committed set. When a [CommitViewProvider] is
// configured, Committers receive a shallow Result copy materialized from
// its projected board; callers and Observers retain the engine result.
// Revise attempts and discarded or non-completed results are never
// committed. Implementations MUST treat
// [Identity.RunID] as the operation's idempotency key because a caller
// may retry after an ambiguous storage or transport failure.
// A Referee's Revise request that the configured budget does not honor
// is advisory and does not by itself clear Committed; Referees that
// require the output to be withheld must also set DiscardOutput.
//
// A Committer MUST honor ctx cancellation and MUST NOT mutate req or
// res. Multiple Committers do not form a transaction: if a later
// Committer fails, earlier ones may already have succeeded. Prefer one
// Committer that writes to a transactional store or durable outbox when
// atomicity across side effects matters.
type Committer interface {
	Commit(ctx context.Context, id Identity, req *Request, res *Result) error
}

// CommitterFunc adapts a function to [Committer].
type CommitterFunc func(ctx context.Context, id Identity, req *Request, res *Result) error

// Commit calls f.
func (f CommitterFunc) Commit(ctx context.Context, id Identity, req *Request, res *Result) error {
	return f(ctx, id, req, res)
}

// commitResult runs Committers in registration order. The first error
// stops the chain and is returned unchanged so callers can classify it.
func commitResult(ctx context.Context, id Identity, req *Request, res *Result, committers []Committer) error {
	for _, c := range committers {
		if c == nil {
			continue
		}
		if err := c.Commit(ctx, id, req, res); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Referee (decision) ----------

// Referee is a decision-making lifecycle hook that can influence what
// agent.Execute does at well-defined boundaries. It is the read-write
// counterpart of [Observer]:
//
//   - Committers persist accepted results and may return errors.
//   - Observers see what happened and emit best-effort side effects.
//   - Referees return a structured decision agent.Run interprets.
//
// Referees expose one decision point — [Referee.After] — which fires
// after Execute returned but before [Run] invokes [Committer] and
// [Observer]. This
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
// boolean fields: any Referee asking to accept, discard, or revise is
// retained. Discard has final precedence when Run applies the merged
// decision. The first non-empty Reason wins, so callers can attribute
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
	// AcceptOutput, when true, positively accepts the engine-produced
	// output regardless of Status. This is primarily used to retain
	// useful partial output from a cooperative interruption through the
	// normal Committer chain.
	//
	// DiscardOutput has final precedence when both directives are
	// present, including when they come from different Referees.
	AcceptOutput bool

	// DiscardOutput, when true, instructs Run to mark Result.Committed
	// = false regardless of Status. Committers are skipped for a
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
	// faulty Referees. An unhonored Revise does not change Committed;
	// set DiscardOutput as well when the current output must not be
	// persisted.
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

	// State carries optional structured metadata the referee wants
	// attached to [Result.State] (e.g. a handoff event). Referees MUST
	// NOT mutate res directly; this is the only channel for adding
	// result metadata. Entries are merged after the referee chain, with
	// later referees winning on key conflicts.
	State map[string]any
}

// IsZero reports whether no referee expressed any opinion.
func (d Decision) IsZero() bool {
	return !d.AcceptOutput && !d.DiscardOutput && !d.Revise &&
		d.Reason == "" && len(d.State) == 0
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
		if d.AcceptOutput {
			merged.AcceptOutput = true
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
		if d.State != nil {
			if merged.State == nil {
				merged.State = make(map[string]any, len(d.State))
			}
			maps.Copy(merged.State, d.State)
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
