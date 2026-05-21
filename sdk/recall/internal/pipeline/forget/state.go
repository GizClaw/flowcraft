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
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// ForgetFilter narrows the set of facts the forget_all stage acts on
// when populated. Without a filter the stage retires every fact in
// the scope (the GDPR ForgetAll path); with one it acts on the
// subset that matches — backing Memory.ExpireRetired (D5
// 2026-05-21) as the TTL-style sweep variant.
//
// ExpiresBefore is the only filter axis today: facts with non-nil
// ExpiresAt that is not after the cutoff time are included. Adding
// more axes (e.g. ClosedBefore) is additive — populate a new field
// and AND it into the filter predicate.
type ForgetFilter struct {
	// ExpiresBefore restricts the include-set to facts whose
	// ExpiresAt is set and not after this cutoff. Zero time
	// disables the axis.
	ExpiresBefore *time.Time
}

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
	// scope.PartitionKey(). The validate step compares it to the
	// computed key and refuses to proceed on mismatch (Hard mode
	// only; Soft is reversible so we skip the guard for ergonomics).
	ConfirmScopeKey string

	// Filter narrows the act-on set to a subset of scope facts.
	// nil = retire everything (ForgetAll); non-nil = filtered
	// sweep (ExpireRetired). When Filter is non-nil the stage
	// implicitly treats Mode as Hard and skips the
	// confirmScopeKey guard (TTL is a non-irreversible delete,
	// not a GDPR full-scope wipe).
	Filter *ForgetFilter

	// Now is the wall-clock anchor stages use to evaluate the
	// Filter predicate. Zero defers to time.Now() inside the
	// stage; explicit callers (TTL sweep callers) populate it so
	// the cutoff is testable.
	Now time.Time

	// Deleted is the stage output — number of facts the run
	// retired. For Soft mode this is the count of Closed=true
	// writes; for Hard mode it is the store's reported delete count.
	Deleted int

	// DeletedFactIDs lists the canonical fact IDs removed or
	// soft-closed in this run. Memory uses it to cancel async
	// semantic jobs that reference those episodes (ExpireRetired /
	// ForgetAll) without wiping unrelated pending jobs.
	DeletedFactIDs []string

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
