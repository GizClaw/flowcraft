// Package rebuild owns the rebuild-flow pipeline State and Runner.
// The scan / project stages live under rebuild/stages/; this package
// owns the State schema so RebuildAll /
// RebuildProjection share one pipeline.
package rebuild

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// RebuildState is the per-call workspace threaded through every
// Stage of the rebuild pipeline. Each stage reads inputs from
// previous fields and populates its own output slot.
//
// Field ownership by stage:
//
//	scan    → Facts (scan TemporalStore, includeSuperseded=true)
//	project → PerProjection (apply each selected projection,
//	          compute drift = PriorEntries != Applied)
//
// The same RebuildState drives both Memory entry points:
//
//   - RebuildAll(ctx, scope)         → ProjectionFilter == ""
//   - RebuildProjection(ctx, scope, name) → ProjectionFilter == name
type RebuildState struct {
	// Inputs — populated by the runner before Run begins.

	// Scope is the canonical scope to rebuild. scan stage walks
	// only this scope; cross-scope rebuilds are caller-driven
	// (iterate scopes, call Run per scope) so the rebuild
	// pipeline itself stays single-scope.
	Scope domain.Scope

	// ProjectionFilter restricts the project stage to a single
	// projection (matched by Projection.Name()). Empty string =
	// rebuild every registered projection (the RebuildAll path).
	ProjectionFilter string

	// BatchSize hints how many facts the scan stage should
	// hand to project at once. Zero means "no batching, project
	// everything in one shot", which is the current implementation.
	// Non-zero is the seam future incremental rebuilds will use
	// without re-shaping State.
	BatchSize int

	// Cursor is the scan stage's resume token for paginated
	// rebuilds. Zero is "start at the beginning"; non-zero is
	// the opaque token a previous scan returned. The current scan
	// stage always returns "" (single-shot), but the slot
	// exists so future stage implementations stay drop-in.
	Cursor string

	// Now is captured once at Pipeline entry so every projection
	// observes the same wall clock when computing ValidFrom /
	// ValidTo windows during reapply.
	Now time.Time

	// Stage outputs — populated in order.

	// Facts is the slice of canonical facts the scan stage
	// loaded (includeSuperseded=true so projections see the full
	// supersede chain). project iterates this once per
	// projection.
	Facts []domain.TemporalFact

	// PerProjection holds one entry per projection the project
	// stage actually invoked. The slice respects the order
	// projections were registered, matching the order
	// diagnostic.RebuildProjectionDetail.ProjectionName events
	// were emitted in.
	PerProjection []ProjectionRebuildResult

	// Trace is the in-flight RebuildTrace. Pipeline.AppendTrace
	// pushes every emitted StageDiagnostic into Trace.Stages.
	// nil is permitted — callers that don't request explain
	// output pay zero allocation.
	Trace *RebuildTrace
}

// ProjectionRebuildResult is the per-projection outcome the
// project stage populates. It mirrors RebuildProjectionDetail one
// for one so the stage can build the Detail directly from this
// struct without an intermediate model.
type ProjectionRebuildResult struct {
	Name          string
	PriorEntries  int
	Applied       int
	Dropped       int
	DriftDetected bool
	Err           string
}

// RebuildTrace is the rebuild-flow trace surface — the parallel of
// domain.RecallTrace / domain.SaveTrace, but local to this package
// because no public API currently returns it. If a public
// RebuildExplain is added, this type is the natural candidate to lift
// into domain/trace.go alongside the other two
// traces (single line, mechanical move — no behavioural change).
type RebuildTrace struct {
	Stages []diagnostic.StageDiagnostic
}

// EnsureTrace allocates the RebuildTrace if it was not pre-
// populated. Idempotent.
func (s *RebuildState) EnsureTrace() *RebuildTrace {
	if s.Trace == nil {
		s.Trace = &RebuildTrace{}
	}
	return s.Trace
}

// AppendStage is the TraceAppender the runner registers with the
// pipeline framework.
func (s *RebuildState) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}

// SelectsProjection reports whether the supplied projection name
// is in scope for this rebuild — i.e. the filter is empty (rebuild
// every projection) or matches name exactly. The project stage
// calls this once per registered projection.
func (s *RebuildState) SelectsProjection(name string) bool {
	if s == nil {
		return false
	}
	return s.ProjectionFilter == "" || s.ProjectionFilter == name
}
