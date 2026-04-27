package agent

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

// DiscardOnInterruptCauses is a [Decider] that asks Run to discard
// the produced output whenever the engine reported an interrupt with
// any of the listed causes. It is the canonical disposition policy
// for voice / streaming UX — a barge-in shouldn't leave half-baked
// assistant text in the transcript.
//
// Construct it with [NewDiscardOnInterruptCauses]; the zero value is
// not useful (no causes match).
//
// # Default behaviour without this Decider
//
// agent.Run already sets Result.Committed=false on every non-completed
// outcome by default, so installing DiscardOnInterruptCauses purely
// for "discard on barge-in" is technically redundant — the default
// would discard anyway. The reason it is still a useful Decider:
//
//   - it sets Result.State["finalize_reason"] to a caller-supplied
//     attribution string, which the default policy cannot do;
//
//   - it makes the policy explicit at the call site so a future
//     change to the default (e.g. "commit interrupted runs by
//     default") would not silently change voice's behaviour.
type DiscardOnInterruptCauses struct {
	causes map[engine.Cause]struct{}
	reason string
}

// NewDiscardOnInterruptCauses returns a Decider that discards output
// for the given engine.Cause set. Reason is recorded in
// Result.State["finalize_reason"] when the decider fires.
//
// Common preset:
//
//	agent.NewDiscardOnInterruptCauses("barge-in",
//	    engine.CauseUserInput, engine.CauseUserCancel)
func NewDiscardOnInterruptCauses(reason string, causes ...engine.Cause) *DiscardOnInterruptCauses {
	d := &DiscardOnInterruptCauses{
		causes: make(map[engine.Cause]struct{}, len(causes)),
		reason: reason,
	}
	for _, c := range causes {
		d.causes[c] = struct{}{}
	}
	return d
}

// BeforeFinalize implements [Decider].
func (d *DiscardOnInterruptCauses) BeforeFinalize(_ context.Context, _ RunInfo, _ *Request, res *Result) (FinalizeDecision, error) {
	if res.Status != StatusInterrupted {
		return FinalizeDecision{}, nil
	}
	if _, ok := d.causes[res.Cause]; !ok {
		return FinalizeDecision{}, nil
	}
	return FinalizeDecision{DiscardOutput: true, Reason: d.reason}, nil
}

var _ Decider = (*DiscardOnInterruptCauses)(nil)
