// Package feedback owns the pipeline that applies caller feedback
// (Reinforce / Penalize) to a canonical fact.
//
// Cluster A (2026-05-21) promoted Reinforce / Penalize into the
// pipeline framework so the per-fact UpdateFeedback write and the
// follow-up single-fact reproject (Cluster D — keeps retrieval Doc
// metadata fresh) emit one observable record per call. Memory.
// Reinforce and Memory.Penalize construct State and invoke
// Runner.Run; they hold the scope write-lock around the call so
// concurrent feedback writes serialise per scope.
package feedback

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// State is the per-call workspace threaded through the feedback
// pipeline. The facade (Memory.Reinforce / Memory.Penalize)
// populates Scope / FactID / Reinforcement+Penalty deltas; the
// apply_feedback stage writes the refreshed canonical fact into
// Updated so the runner can hand a typed snapshot back (today the
// facade discards it — kept for symmetry with the write pipeline
// State shape and to keep stage tests asserting on observable
// fields).
type State struct {
	Scope              domain.Scope
	FactID             string
	ReinforcementDelta float64
	PenaltyDelta       float64

	// Updated is the post-UpdateFeedback snapshot. apply_feedback
	// re-reads it from the store before reprojecting so downstream
	// projections see the freshly-clamped reinforcement / penalty
	// values rather than the caller's raw deltas.
	Updated domain.TemporalFact

	// Trace mirrors the other pipeline trace shapes. nil = caller
	// did not request explain output (zero allocation).
	Trace *Trace
}

// Trace carries the apply_feedback stage diagnostic. Kept local
// because no public API currently returns it; lift to a domain-
// owned trace when a public Memory.ReinforceExplain lands.
type Trace struct {
	Stages []diagnostic.StageDiagnostic
}

// EnsureTrace allocates the Trace if not pre-populated. Idempotent.
func (s *State) EnsureTrace() *Trace {
	if s.Trace == nil {
		s.Trace = &Trace{}
	}
	return s.Trace
}

// AppendStage is the TraceAppender registered with the pipeline
// framework. It is a no-op when Trace is nil so callers that only
// want the side effects pay zero allocation.
func (s *State) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}
