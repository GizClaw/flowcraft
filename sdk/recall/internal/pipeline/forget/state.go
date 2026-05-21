// Package forget owns the scope-wide retirement pipeline (Phase D.8 C9 —
// GDPR Art.17 / CCPA 1798.105 compliance). It mirrors the rebuild
// pipeline shape: one State, one Runner, one stage (forget_all).
//
// ForgetAll is a single-stage pipeline by design — the operation is
// inherently atomic from the caller's point of view (one scope, one
// outcome), and splitting into list / mark_closed / clear_projections /
// delete_store would expose intermediate states that have no
// independent observability value. Carrying it as one stage keeps
// trace.Stages readable and the ForgetAllDetail counters comprehensive.
package forget

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// State is the per-call workspace for the forget pipeline. The
// caller (Memory.ForgetAll) populates Scope / Mode / ConfirmScopeKey
// before Run; the stage writes Deleted into State so the facade can
// return it to the caller.
type State struct {
	// Scope is the primary scope whose facts must be retired.
	// Federation sub-scopes on this Scope are intentionally IGNORED
	// by the forget_all stage — ForgetAll is non-recursive (D.5
	// "Federation 中仅作用于参数 scope").
	Scope domain.Scope

	// Mode selects soft (Closed=true, store retained) vs hard
	// (physical delete + projection clear + evidence clear).
	Mode domain.ForgetMode

	// ConfirmScopeKey is the caller's defensive copy of
	// scope.CanonicalKey(). The validate step compares it to the
	// computed key and refuses to proceed on mismatch (Hard mode
	// only; Soft is reversible so we skip the guard for ergonomics).
	ConfirmScopeKey string

	// Deleted is the stage output — number of facts the run
	// retired. For Soft mode this is the count of Closed=true
	// writes; for Hard mode it is the store's reported delete count.
	Deleted int

	// Trace mirrors the rebuild pipeline trace shape. nil = caller
	// did not request explain output (zero allocation).
	Trace *Trace
}

// Trace carries the forget_all stage diagnostic. Kept local because
// no public API currently returns it; lift to domain/trace.go when
// Memory.ForgetAllExplain lands.
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
// framework.
func (s *State) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}
