package agent

import "context"

// ---------- Hook (observer) ----------

// Hook is a read-only lifecycle hook that lets callers react to
// stages of a [Run] without affecting its outcome. It is the plumbing
// behind agent's "history append on completion", "metric emit on
// start", "transcript snapshot on interrupt", and similar patterns,
// none of which agent hard-codes any more.
//
// Design rules:
//
//  1. Hooks MUST NOT change the [Result] returned by Run. agent
//     intentionally exposes the Result to OnRunEnd by pointer because
//     it is the same value the caller will receive — observers may
//     stash references to it (for logging, async append, …) but
//     mutating it leaves agent's caller staring at the mutation. Treat
//     this surface as advisory.
//
//  2. Hook methods MUST NOT return an error. Failures inside an
//     observer are the observer's problem; they MUST NOT propagate
//     into Run. When an observer needs to fail or alter a turn (guard
//     hooks, moderation, disposition), use a [AfterExecute] instead — its
//     explicit decision semantics keep the flow auditable.
//
//  3. Hook methods are called synchronously from Run on the
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
// Embed [BaseHook] to satisfy the interface with no-op defaults
// when only a subset of the methods are interesting.
type Hook interface {
	// OnRunStart fires after Run prepared the engine inputs but
	// before Execute is invoked. id carries the immutable
	// identification fields agreed for this turn.
	OnRunStart(ctx context.Context, id Identity, req *Request)

	// OnInterrupt fires only when the engine returned an interrupt
	// error. It runs before OnRunEnd. intr carries the structured
	// reason supplied by the host.
	OnInterrupt(ctx context.Context, id Identity, intr Interrupt)

	// OnRunRevise fires when a AfterExecute asked agent.Execute to re-invoke
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
	// OnRunRevise is the canonical hook for "log how many times the
	// answer needed revision" / "page on excessive revise loops" /
	// "snapshot intermediate boards before they are discarded". It
	// fires zero times for runs that complete on the first attempt
	// or whose AfterExecute never asks for revise.
	OnRunRevise(ctx context.Context, id Identity, prevRes *Result, nextAttempt int)

	// OnRunEnd fires after Execute returned and Run finished
	// classifying the outcome. res is the same pointer Run is about
	// to return; observers MUST treat it as read-only.
	OnRunEnd(ctx context.Context, id Identity, res *Result)
}

// BaseHook provides no-op default implementations of every
// Hook method. Embed it in custom observers that only care about a
// subset of the lifecycle:
//
//	type historyAppender struct {
//	    agent.BaseHook
//	    store sdk_history.History
//	}
//
//	func (h *historyAppender) OnRunEnd(ctx context.Context, id agent.Identity, res *agent.Result) {
//	    if res.Status != agent.StatusCompleted { return }
//	    _ = h.store.Append(ctx, id.ContextID, res.Messages)
//	}
type BaseHook struct{}

// OnRunStart is a no-op.
func (BaseHook) OnRunStart(context.Context, Identity, *Request) {}

// OnInterrupt is a no-op.
func (BaseHook) OnInterrupt(context.Context, Identity, Interrupt) {}

// OnRunRevise is a no-op.
func (BaseHook) OnRunRevise(context.Context, Identity, *Result, int) {}

// OnRunEnd is a no-op.
func (BaseHook) OnRunEnd(context.Context, Identity, *Result) {}

// Compile-time assertion BaseHook satisfies Hook.
var _ Hook = BaseHook{}

// composeHooks returns a single Hook that fans every method
// out to obs in registration order, swallowing panics so one bad
// observer cannot tear down the run loop. nil entries are skipped.
//
// Returns nil when obs is empty so callers can branch on
// "no observers" without paying the dispatch cost.
func composeHooks(obs []Hook) Hook {
	filtered := obs[:0:0]
	for _, o := range obs {
		if o != nil {
			filtered = append(filtered, o)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return multiHook(filtered)
}

type multiHook []Hook

func (m multiHook) OnRunStart(ctx context.Context, id Identity, req *Request) {
	for _, o := range m {
		safeRun(func() { o.OnRunStart(ctx, id, req) })
	}
}

func (m multiHook) OnInterrupt(ctx context.Context, id Identity, intr Interrupt) {
	for _, o := range m {
		safeRun(func() { o.OnInterrupt(ctx, id, intr) })
	}
}

func (m multiHook) OnRunRevise(ctx context.Context, id Identity, prev *Result, next int) {
	for _, o := range m {
		safeRun(func() { o.OnRunRevise(ctx, id, prev, next) })
	}
}

func (m multiHook) OnRunEnd(ctx context.Context, id Identity, res *Result) {
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

// ---------- BeforeExecute ----------

// BeforeExecute builds the initial [Board] for a run.
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
//   - Before is called exactly once per Run, before
//     Execute and before any Hook's OnRunStart.
//   - The returned board is mutated by the engine; Before must
//     therefore return a fresh value each call (do NOT cache and
//     re-yield a single Board).
//   - The returned board MUST be non-nil. Returning nil is a Run
//     infrastructure error.
//
// Implementations are expected to be cheap and synchronous; long
// async work (retrieval, IO) belongs in a wrapper that resolves
// before Run.
type BeforeExecute interface {
	Before(ctx context.Context, id Identity, req *Request) (*Board, error)
}

// BeforeExecuteFunc is the function-typed adapter for BeforeExecute.
//
// Useful when the seed logic is a single closure over a transcript
// loader or retriever:
//
//	agent.WithBeforeExecute(agent.BeforeExecuteFunc(func(ctx context.Context, id agent.Identity, req *agent.Request) (*Board, error) {
//	    prior, err := store.Load(ctx, id.ContextID)
//	    if err != nil { return nil, err }
//	    b := NewBoard()
//	    b.SetChannel(MainChannel, prior)
//	    b.AppendChannelMessage(MainChannel, req.Message)
//	    return b, nil
//	}))
type BeforeExecuteFunc func(ctx context.Context, id Identity, req *Request) (*Board, error)

// Before calls f.
func (f BeforeExecuteFunc) Before(ctx context.Context, id Identity, req *Request) (*Board, error) {
	return f(ctx, id, req)
}

// defaultBefore is the seed Run uses when [WithBeforeExecute] is not
// configured. It produces a fresh board, appends req.Message to
// MainChannel, and copies req.Inputs into board vars. It does NOT
// load any history; that is a deliberate choice — agents that need
// transcript continuity wire it through a custom BeforeExecute (most
// often a thin closure around memory/history).
type defaultBefore struct{}

// Before implements [BeforeExecute].
func (defaultBefore) Before(_ context.Context, _ Identity, req *Request) (*Board, error) {
	b := NewBoard()
	b.AppendChannelMessage(MainChannel, req.Message)
	for k, v := range req.Inputs {
		b.SetVar(k, v)
	}
	return b, nil
}

var _ BeforeExecute = defaultBefore{}
var _ BeforeExecute = BeforeExecuteFunc(nil)

// ---------- AfterExecute ----------

// AfterExecute is a decision-making lifecycle hook that can influence what
// agent.Execute does at well-defined boundaries. It is the read-write
// counterpart of [Hook]:
//
//   - Hooks see what happened and emit side effects (logs,
//     metrics, transcript persistence).
//   - After return a structured decision agent.Run interprets.
//
// Round B exposes one decision point — [AfterExecute.After] —
// which fires after Execute returned but before [Run] commits
// the produced messages to history (i.e., before any Hook's
// OnRunEnd). This covers two real cases:
//
//  1. Disposition: a barge-in cause means the assistant was cut off
//     mid-thought; the half-baked output should not appear in the
//     persistent transcript. A AfterExecute returns
//     Decision{DiscardOutput: true}.
//
//  2. (Reserved) Revise: the natural answer fails some quality bar
//     (no citations, policy violation, refusal-without-reason); the
//     AfterExecute asks for one more model pass. The wire field is
//     present for forward compatibility — agent does not yet honour
//     it, and engines will need explicit support before it has any
//     effect.
//
// # Composition
//
// Multiple After may be registered (Agent-scoped + per-call).
// They run in registration order. The merged decision is the OR over
// boolean fields: any AfterExecute asking to discard wins; same for
// revise. The first non-empty Reason wins, so callers can attribute
// the decision in logs.
//
// # Error contract
//
// A AfterExecute returning a non-nil error short-circuits the merge and
// causes Run to return (Result, decider-error). agent does NOT swap
// the error class — it surfaces the AfterExecute's own error so callers
// can classify with errdefs. The Result is still populated
// (including the engine's output) so the caller can decide what to
// do next.
//
// Embed [BaseAfterExecute] to satisfy the interface with no-op defaults.
type AfterExecute interface {
	// After fires after Execute returns. The AfterExecute
	// inspects res (read-only) and the original req, and returns a
	// Decision that Run merges with other After' decisions.
	//
	// id carries the immutable identification fields agreed for
	// this turn. The AfterExecute MUST NOT mutate res; agent will surface
	// the merged decision via [Result.Committed] and (when a Reason
	// was supplied) [Result.State]["finalize_reason"].
	After(ctx context.Context, id Identity, req *Request, res *Result) (Decision, error)
}

// Decision is the return type of [AfterExecute.After]. The
// zero value means "no opinion" — agent applies its defaults.
//
// Defaults agent.Run uses when no AfterExecute returns a directive:
//
//   - StatusCompleted runs are committed.
//   - StatusInterrupted / StatusCanceled / StatusAborted /
//     StatusFailed runs are NOT committed (their partial output is
//     dropped from the transcript view). This matches the
//     conservative behaviour Round A had hard-coded; round B simply
//     makes it overridable.
type Decision struct {
	// DiscardOutput, when true, instructs Run to mark Result.Committed
	// = false regardless of Status. Hooks reading Committed
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
	// faulty After.
	//
	// When honoured the lifecycle is:
	//
	//   1. AfterExecute returns Revise=true (and optionally Reason).
	//   2. Run fires [Hook.OnRunRevise] with the about-to-be-
	//      discarded Result and the next attempt index.
	//   3. Board is re-seeded via the configured BeforeExecute; the
	//      same Run identifier is reused so observers that
	//      key by run id can correlate attempts.
	//   4. Execute runs again. The AfterExecute chain runs again
	//      on the new Result.
	//   5. The loop exits when either Revise=false or the attempt
	//      counter reaches WithMaxRevise. The final Result.Attempts
	//      reflects how many Execute calls were made.
	//
	// Revise interacts with [WithResumeFrom]: ResumeFrom applies
	// to the FIRST attempt only. Revise restarts are fresh runs
	// (the engine should be re-entered from the start), so
	// subsequent attempts drop ResumeFrom — replaying a checkpoint
	// repeatedly would defeat the purpose of asking for a revision.
	Revise bool

	// Reason is a free-form short string explaining the decision.
	// Agent stores the first non-empty Reason in Result.State under
	// "finalize_reason" so logs / metrics can attribute the
	// outcome.
	Reason string
}

// merge folds other into d using the Round B rules: OR over booleans,
// first non-empty Reason wins.
func (d Decision) merge(other Decision) Decision {
	d.DiscardOutput = d.DiscardOutput || other.DiscardOutput
	d.Revise = d.Revise || other.Revise
	if d.Reason == "" {
		d.Reason = other.Reason
	}
	return d
}

// BaseAfterExecute provides a no-op default implementation of every
// AfterExecute method. Embed it when only a subset of decision points
// matter.
type BaseAfterExecute struct{}

// After returns the zero-value Decision (no
// opinion).
func (BaseAfterExecute) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return Decision{}, nil
}

var _ AfterExecute = BaseAfterExecute{}

// runAfterExecute executes all After in order, merges their decisions,
// and returns the combined result. The first error short-circuits
// the merge.
func runAfterExecute(ctx context.Context, ds []AfterExecute, id Identity, req *Request, res *Result) (Decision, error) {
	var out Decision
	for _, d := range ds {
		if d == nil {
			continue
		}
		dec, err := d.After(ctx, id, req, res)
		if err != nil {
			return out, err
		}
		out = out.merge(dec)
	}
	return out, nil
}

// ---------- Dispositions ----------

// DiscardOnInterruptCauses is a [AfterExecute] that asks Run to discard
// the produced output whenever the engine reported an interrupt with
// any of the listed causes. It is the canonical disposition policy
// for voice / streaming UX — a barge-in shouldn't leave half-baked
// assistant text in the transcript.
//
// Construct it with [NewDiscardOnInterruptCauses]; the zero value is
// not useful (no causes match).
//
// # Default behaviour without this AfterExecute
//
// agent.Run already sets Result.Committed=false on every non-completed
// outcome by default, so installing DiscardOnInterruptCauses purely
// for "discard on barge-in" is technically redundant — the default
// would discard anyway. The reason it is still a useful AfterExecute:
//
//   - it sets Result.State["finalize_reason"] to a caller-supplied
//     attribution string, which the default policy cannot do;
//
//   - it makes the policy explicit at the call site so a future
//     change to the default (e.g. "commit interrupted runs by
//     default") would not silently change voice's behaviour.
type DiscardOnInterruptCauses struct {
	causes map[Cause]struct{}
	reason string
}

// NewDiscardOnInterruptCauses returns a AfterExecute that discards output
// for the given Cause set. Reason is recorded in
// Result.State["finalize_reason"] when the decider fires.
//
// Common preset:
//
//	agent.NewDiscardOnInterruptCauses("barge-in",
//	    CauseUserInput, CauseUserCancel)
func NewDiscardOnInterruptCauses(reason string, causes ...Cause) *DiscardOnInterruptCauses {
	d := &DiscardOnInterruptCauses{
		causes: make(map[Cause]struct{}, len(causes)),
		reason: reason,
	}
	for _, c := range causes {
		d.causes[c] = struct{}{}
	}
	return d
}

// After implements [AfterExecute].
func (d *DiscardOnInterruptCauses) After(_ context.Context, _ Identity, _ *Request, res *Result) (Decision, error) {
	if res.Status != StatusInterrupted {
		return Decision{}, nil
	}
	if _, ok := d.causes[res.Cause]; !ok {
		return Decision{}, nil
	}
	return Decision{DiscardOutput: true, Reason: d.reason}, nil
}

var _ AfterExecute = (*DiscardOnInterruptCauses)(nil)
