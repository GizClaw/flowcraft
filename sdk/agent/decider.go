package agent

import "context"

// Decider is a decision-making lifecycle hook that can influence what
// agent.Run does at well-defined boundaries. It is the read-write
// counterpart of [Observer]:
//
//   - Observers see what happened and emit side effects (logs,
//     metrics, transcript persistence).
//   - Deciders return a structured decision agent.Run interprets.
//
// Round B exposes one decision point — [Decider.BeforeFinalize] —
// which fires after engine.Execute returned but before [Run] commits
// the produced messages to history (i.e., before any Observer's
// OnRunEnd). This covers two real cases:
//
//  1. Disposition: a barge-in cause means the assistant was cut off
//     mid-thought; the half-baked output should not appear in the
//     persistent transcript. A Decider returns
//     FinalizeDecision{DiscardOutput: true}.
//
//  2. (Reserved) Revise: the natural answer fails some quality bar
//     (no citations, policy violation, refusal-without-reason); the
//     Decider asks for one more model pass. The wire field is
//     present for forward compatibility — agent does not yet honour
//     it, and engines will need explicit support before it has any
//     effect.
//
// # Composition
//
// Multiple Deciders may be registered (Agent-scoped + per-call).
// They run in registration order. The merged decision is the OR over
// boolean fields: any Decider asking to discard wins; same for
// revise. The first non-empty Reason wins, so callers can attribute
// the decision in logs.
//
// # Error contract
//
// A Decider returning a non-nil error short-circuits the merge and
// causes Run to return (Result, decider-error). agent does NOT swap
// the error class — it surfaces the Decider's own error so callers
// can classify with errdefs. The Result is still populated
// (including the engine's output) so the caller can decide what to
// do next.
//
// Embed [BaseDecider] to satisfy the interface with no-op defaults.
type Decider interface {
	// BeforeFinalize fires after engine.Execute returns. The Decider
	// inspects res (read-only) and the original req, and returns a
	// FinalizeDecision that Run merges with other Deciders' decisions.
	//
	// info carries the immutable identification fields agreed for
	// this turn. The Decider MUST NOT mutate res; agent will surface
	// the merged decision via [Result.Committed] and (when a Reason
	// was supplied) [Result.State]["finalize_reason"].
	BeforeFinalize(ctx context.Context, info RunInfo, req *Request, res *Result) (FinalizeDecision, error)
}

// FinalizeDecision is the return type of [Decider.BeforeFinalize]. The
// zero value means "no opinion" — agent applies its defaults.
//
// Defaults agent.Run uses when no Decider returns a directive:
//
//   - StatusCompleted runs are committed.
//   - StatusInterrupted / StatusCanceled / StatusAborted /
//     StatusFailed runs are NOT committed (their partial output is
//     dropped from the transcript view). This matches the
//     conservative behaviour Round A had hard-coded; round B simply
//     makes it overridable.
type FinalizeDecision struct {
	// DiscardOutput, when true, instructs Run to mark Result.Committed
	// = false regardless of Status. Observers reading Committed
	// (notably history-append observers) skip persistence on a
	// discarded run.
	//
	// Setting DiscardOutput on a StatusCompleted run is allowed and
	// useful for moderation hooks ("the answer violates policy, do
	// not persist it").
	DiscardOutput bool

	// Revise is reserved for round B+ engine support. When true,
	// agent will (eventually) re-invoke the engine with the same
	// inputs. Round B does NOT yet honour Revise; it is included so
	// the FinalizeDecision shape does not need to break when the
	// behaviour is wired up.
	Revise bool

	// Reason is a free-form short string explaining the decision.
	// Agent stores the first non-empty Reason in Result.State under
	// "finalize_reason" so logs / metrics can attribute the
	// outcome.
	Reason string
}

// merge folds other into d using the Round B rules: OR over booleans,
// first non-empty Reason wins.
func (d FinalizeDecision) merge(other FinalizeDecision) FinalizeDecision {
	d.DiscardOutput = d.DiscardOutput || other.DiscardOutput
	d.Revise = d.Revise || other.Revise
	if d.Reason == "" {
		d.Reason = other.Reason
	}
	return d
}

// BaseDecider provides a no-op default implementation of every
// Decider method. Embed it when only a subset of decision points
// matter.
type BaseDecider struct{}

// BeforeFinalize returns the zero-value FinalizeDecision (no
// opinion).
func (BaseDecider) BeforeFinalize(context.Context, RunInfo, *Request, *Result) (FinalizeDecision, error) {
	return FinalizeDecision{}, nil
}

var _ Decider = BaseDecider{}

// runDeciders executes all Deciders in order, merges their decisions,
// and returns the combined result. The first error short-circuits
// the merge.
func runDeciders(ctx context.Context, ds []Decider, info RunInfo, req *Request, res *Result) (FinalizeDecision, error) {
	var out FinalizeDecision
	for _, d := range ds {
		if d == nil {
			continue
		}
		dec, err := d.BeforeFinalize(ctx, info, req, res)
		if err != nil {
			return out, err
		}
		out = out.merge(dec)
	}
	return out, nil
}
