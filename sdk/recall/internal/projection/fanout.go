package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Op identifies the canonical write operation that triggered a
// projection call. Telemetry hooks use it to attribute failures to
// the right phase of the write path.
type Op string

const (
	OpProject Op = "project"
	OpForget  Op = "forget"
	OpRebuild Op = "rebuild"
)

// TelemetryHook receives lifecycle and failure signals from the
// fanout and from drift-aware components (notably materialize). PR-2
// ships a NopTelemetry default — a real telemetry package wires in
// during Phase 8.
//
// Both methods are required so the interface stays one-shot to
// implement; NopTelemetry provides empty implementations that the
// compiler can embed when a partial hook is needed.
type TelemetryHook interface {
	OnProjection(event ProjectionEvent)
	OnDrift(event DriftEvent)
}

// ProjectionEvent carries enough context for a telemetry backend to
// attribute a fanout outcome. Err is nil on success.
type ProjectionEvent struct {
	Projection  string
	Op          Op
	Consistency Consistency
	FactCount   int
	Err         error
}

// DriftReason classifies a single projection-vs-canonical drift
// observation. The set is intentionally narrow in PR-5; reverse
// drift detection (projection has an id the store does not)
// arrives with Phase 8 governance.
type DriftReason string

const (
	// DriftStaleFact is emitted when a candidate references a
	// fact id that the canonical store no longer knows about —
	// typically a retrieval projection that still holds a doc
	// after the underlying fact has been Forget()'d.
	DriftStaleFact DriftReason = "stale_fact"

	// DriftSupersededFact is emitted when materialize loads a
	// fact whose CorrectedBy is non-empty. The candidate carries
	// outdated state; the projection should be repaired (see
	// Memory.RepairStale) once a write-path supersede ships the
	// successor revision.
	DriftSupersededFact DriftReason = "superseded_fact"
)

// DriftEvent describes a single drift observation. PR-5 fires these
// from materialize (the single read-path chokepoint); rebuild /
// reconcile-style emitters may join later.
type DriftEvent struct {
	// Scope of the query that surfaced the drift.
	Scope model.Scope
	// Source is the subsystem that detected the drift — e.g.
	// "materialize". Free-form so reverse-drift detectors can
	// label themselves without bumping the type.
	Source string
	// Reason classifies the drift; see DriftReason.
	Reason DriftReason
	// FactID identifies the offending canonical fact (the id the
	// projection still believes is current).
	FactID string
	// Details is an optional human-readable hint (e.g. fusion
	// source name, candidate score) — never load-bearing.
	Details string
}

// NopTelemetry is the zero-cost telemetry hook used until Phase 8.
// Embed it to satisfy TelemetryHook when a caller only cares about
// one event type.
type NopTelemetry struct{}

func (NopTelemetry) OnProjection(ProjectionEvent) {}
func (NopTelemetry) OnDrift(DriftEvent)           {}

// Fanout drives required + optional projections.
//
// The Memory facade orchestrates write-path transactions on top of
// the split-required/optional API (ProjectRequired/ProjectOptional,
// ForgetRequired/ForgetOptional, RebuildRequired/RebuildOptional).
// Required failures must abort the caller's call so canonical state
// and projections stay aligned; optional failures only emit
// telemetry.
//
// The convenience Project/Forget/Rebuild helpers run required then
// optional in one call without compensation; production code uses
// the split API so it can perform best-effort rollback / forward
// compensation between phases.
type Fanout struct {
	required  []Projection
	optional  []Projection
	telemetry TelemetryHook
}

// New constructs a Fanout. Projections are partitioned by their
// declared Consistency so the fanout itself stays oblivious to
// individual projection types.
func New(projections []Projection, hook TelemetryHook) *Fanout {
	if hook == nil {
		hook = NopTelemetry{}
	}
	f := &Fanout{telemetry: hook}
	for _, p := range projections {
		if p == nil {
			continue
		}
		switch p.Consistency() {
		case Required:
			f.required = append(f.required, p)
		case Optional:
			f.optional = append(f.optional, p)
		}
	}
	return f
}

// ProjectRequired runs required projections synchronously. The first
// failure short-circuits and is returned wrapped with the failing
// projection's name. The caller is responsible for compensating
// already-succeeded projections; the fanout does not auto-rollback.
func (f *Fanout) ProjectRequired(ctx context.Context, facts []model.TemporalFact) error {
	if f == nil || len(facts) == 0 || len(f.required) == 0 {
		return nil
	}
	return f.runRequired(ctx, OpProject, len(facts), func(p Projection) error {
		return p.Project(ctx, facts)
	})
}

// ProjectOptional runs optional projections best-effort. Failures
// only emit telemetry; the call always returns nil.
func (f *Fanout) ProjectOptional(ctx context.Context, facts []model.TemporalFact) {
	if f == nil || len(facts) == 0 || len(f.optional) == 0 {
		return
	}
	f.runOptional(ctx, OpProject, len(facts), func(p Projection) error {
		return p.Project(ctx, facts)
	})
}

// ForgetRequired runs required forgets synchronously. First failure
// short-circuits with an error. Callers must hold the canonical
// fact snapshot if they need compensation across this boundary.
func (f *Fanout) ForgetRequired(ctx context.Context, scope model.Scope, factIDs []string) error {
	if f == nil || len(factIDs) == 0 || len(f.required) == 0 {
		return nil
	}
	return f.runRequired(ctx, OpForget, len(factIDs), func(p Projection) error {
		return p.Forget(ctx, scope, factIDs)
	})
}

// ForgetOptional runs optional forgets best-effort.
func (f *Fanout) ForgetOptional(ctx context.Context, scope model.Scope, factIDs []string) {
	if f == nil || len(factIDs) == 0 || len(f.optional) == 0 {
		return
	}
	f.runOptional(ctx, OpForget, len(factIDs), func(p Projection) error {
		return p.Forget(ctx, scope, factIDs)
	})
}

// RebuildRequired rebuilds required projections from the supplied
// snapshot. Same short-circuit semantics as ProjectRequired.
func (f *Fanout) RebuildRequired(ctx context.Context, scope model.Scope, facts []model.TemporalFact) error {
	if f == nil {
		return nil
	}
	return f.runRequired(ctx, OpRebuild, len(facts), func(p Projection) error {
		return p.Rebuild(ctx, scope, facts)
	})
}

// RebuildOptional rebuilds optional projections best-effort.
func (f *Fanout) RebuildOptional(ctx context.Context, scope model.Scope, facts []model.TemporalFact) {
	if f == nil {
		return
	}
	f.runOptional(ctx, OpRebuild, len(facts), func(p Projection) error {
		return p.Rebuild(ctx, scope, facts)
	})
}

// Project is a convenience helper: required then optional in one
// call without compensation. Production write paths should use the
// split ProjectRequired/ProjectOptional pair so they can rollback
// between phases.
func (f *Fanout) Project(ctx context.Context, facts []model.TemporalFact) error {
	if err := f.ProjectRequired(ctx, facts); err != nil {
		return err
	}
	f.ProjectOptional(ctx, facts)
	return nil
}

// Forget is the unsplit convenience helper.
func (f *Fanout) Forget(ctx context.Context, scope model.Scope, factIDs []string) error {
	if err := f.ForgetRequired(ctx, scope, factIDs); err != nil {
		return err
	}
	f.ForgetOptional(ctx, scope, factIDs)
	return nil
}

// Rebuild is the unsplit convenience helper.
func (f *Fanout) Rebuild(ctx context.Context, scope model.Scope, facts []model.TemporalFact) error {
	if err := f.RebuildRequired(ctx, scope, facts); err != nil {
		return err
	}
	f.RebuildOptional(ctx, scope, facts)
	return nil
}

// RequiredNames returns the names of registered required
// projections, in registration order. Used by compensation logic
// that needs stable iteration without exposing the projection slice.
func (f *Fanout) RequiredNames() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.required))
	for _, p := range f.required {
		out = append(out, p.Name())
	}
	return out
}

// Telemetry exposes the configured hook so the Memory facade can
// emit compensation-stage events under a shared sink.
func (f *Fanout) Telemetry() TelemetryHook {
	if f == nil {
		return NopTelemetry{}
	}
	return f.telemetry
}

func (f *Fanout) runRequired(ctx context.Context, op Op, n int, call func(Projection) error) error {
	for _, p := range f.required {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := call(p)
		f.telemetry.OnProjection(ProjectionEvent{
			Projection:  p.Name(),
			Op:          op,
			Consistency: Required,
			FactCount:   n,
			Err:         err,
		})
		if err != nil {
			return fmt.Errorf("recall projection %q (%s): %w", p.Name(), op, err)
		}
	}
	return nil
}

func (f *Fanout) runOptional(ctx context.Context, op Op, n int, call func(Projection) error) {
	for _, p := range f.optional {
		if err := ctx.Err(); err != nil {
			f.telemetry.OnProjection(ProjectionEvent{
				Projection:  p.Name(),
				Op:          op,
				Consistency: Optional,
				FactCount:   n,
				Err:         err,
			})
			continue
		}
		err := call(p)
		f.telemetry.OnProjection(ProjectionEvent{
			Projection:  p.Name(),
			Op:          op,
			Consistency: Optional,
			FactCount:   n,
			Err:         err,
		})
	}
}

// ErrProjectionDisabled is returned by helpers that resolve a
// projection by name when the projection has not been registered.
var ErrProjectionDisabled = errors.New("recall projection: not registered")
