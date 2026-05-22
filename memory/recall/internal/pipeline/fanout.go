package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/telemetry"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// fanoutOp labels a projection operation for fanout's internal error
// wrapping. Phase E.3 removed the legacy OnProjection telemetry
// channel, so the op label is now an internal pipeline detail; it is
// no longer emitted across the TelemetryHook boundary.
type fanoutOp string

const (
	opProject fanoutOp = "project"
	opForget  fanoutOp = "forget"
	opRebuild fanoutOp = "rebuild"
)

// Fanout drives required + optional projection writes.
//
// Each lens owns its own projection.go (see internal/lens/<name>/);
// Fanout is the L3 pipeline coordinator that dispatches a single
// write to every registered projection, partitioned by their
// declared port.Consistency. Required failures abort the caller so
// canonical state and projections stay aligned; optional failures
// only emit telemetry.
//
// The convenience Project/Forget/Rebuild helpers run required then
// optional in one call without compensation; production code uses
// the split API so it can perform best-effort rollback / forward
// compensation between phases.
type Fanout struct {
	required  []port.Projection
	optional  []port.Projection
	telemetry port.TelemetryHook
}

// NewFanout constructs a Fanout. Projections are partitioned by
// their declared Consistency so the fanout itself stays oblivious to
// individual projection types.
func NewFanout(projections []port.Projection, hook port.TelemetryHook) *Fanout {
	if hook == nil {
		hook = telemetry.NopHook{}
	}
	f := &Fanout{telemetry: hook}
	for _, p := range projections {
		if p == nil {
			continue
		}
		switch p.Consistency() {
		case port.Required:
			f.required = append(f.required, p)
		case port.Optional:
			f.optional = append(f.optional, p)
		}
	}
	return f
}

// ProjectRequired runs required projections synchronously. The first
// failure short-circuits and is returned wrapped with the failing
// projection's name. The caller is responsible for compensating
// already-succeeded projections; the fanout does not auto-rollback.
func (f *Fanout) ProjectRequired(ctx context.Context, facts []domain.TemporalFact) error {
	if f == nil || len(facts) == 0 || len(f.required) == 0 {
		return nil
	}
	return f.runRequired(ctx, opProject, func(p port.Projection) error {
		return p.Project(ctx, facts)
	})
}

// ProjectRequiredForKinds runs required projections synchronously
// against facts filtered to a set of allowed FactKinds.
//
// Projections that implement port.KindFilteredProjection participate
// only when at least one of the allowed kinds is accepted. Projections
// that do NOT implement the optional interface are always included,
// preserving backward compatibility with the unfiltered required tier.
//
// The Phase F.1 sync episode lane uses ProjectRequiredForKindsStrict
// instead so extra required projections must opt in via
// KindFilteredProjection.
func (f *Fanout) ProjectRequiredForKinds(ctx context.Context, facts []domain.TemporalFact, kinds ...domain.FactKind) error {
	if f == nil || len(facts) == 0 || len(f.required) == 0 {
		return nil
	}
	for _, p := range f.required {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !projectionAcceptsAny(p, kinds) {
			continue
		}
		if err := p.Project(ctx, facts); err != nil {
			return fmt.Errorf("recall projection %q (%s): %w", p.Name(), opProject, err)
		}
	}
	return nil
}

// ProjectRequiredForKindsStrict runs required projections that
// implement port.KindFilteredProjection and explicitly accept at
// least one of the allowed kinds. Projections without the interface
// are skipped — used by the F.1 episode lane so custom required
// projections cannot pull remote IO into the scope write lock.
func (f *Fanout) ProjectRequiredForKindsStrict(ctx context.Context, facts []domain.TemporalFact, kinds ...domain.FactKind) error {
	if f == nil || len(facts) == 0 || len(f.required) == 0 || len(kinds) == 0 {
		return nil
	}
	for _, p := range f.required {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !projectionAcceptsAnyStrict(p, kinds) {
			continue
		}
		if err := p.Project(ctx, facts); err != nil {
			return fmt.Errorf("recall projection %q (%s): %w", p.Name(), opProject, err)
		}
	}
	return nil
}

// ProjectionAcceptsKindStrict reports whether p explicitly opts into
// kind via port.KindFilteredProjection. Used by the episode lane
// compensator to match ProjectRequiredForKindsStrict.
func ProjectionAcceptsKindStrict(p port.Projection, kind domain.FactKind) bool {
	filt, ok := p.(port.KindFilteredProjection)
	if !ok {
		return false
	}
	return filt.AcceptsKind(kind)
}

// projectionAcceptsAny reports whether p should participate for the
// supplied allowed kinds. Projections without the optional interface
// always participate. An empty kinds slice means "no restriction" and
// also includes the projection.
func projectionAcceptsAny(p port.Projection, kinds []domain.FactKind) bool {
	filt, ok := p.(port.KindFilteredProjection)
	if !ok {
		return true
	}
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if filt.AcceptsKind(k) {
			return true
		}
	}
	return false
}

func projectionAcceptsAnyStrict(p port.Projection, kinds []domain.FactKind) bool {
	filt, ok := p.(port.KindFilteredProjection)
	if !ok || len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if filt.AcceptsKind(k) {
			return true
		}
	}
	return false
}

// ProjectOptional runs optional projections best-effort. Failures
// only emit telemetry; the call always returns nil.
func (f *Fanout) ProjectOptional(ctx context.Context, facts []domain.TemporalFact) {
	if f == nil || len(facts) == 0 || len(f.optional) == 0 {
		return
	}
	f.runOptional(ctx, opProject, func(p port.Projection) error {
		return p.Project(ctx, facts)
	})
}

// ForgetRequired runs required forgets synchronously. First failure
// short-circuits with an error. Callers must hold the canonical
// fact snapshot if they need compensation across this boundary.
func (f *Fanout) ForgetRequired(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if f == nil || len(factIDs) == 0 || len(f.required) == 0 {
		return nil
	}
	return f.runRequired(ctx, opForget, func(p port.Projection) error {
		return p.Forget(ctx, scope, factIDs)
	})
}

// ForgetOptional runs optional forgets best-effort.
func (f *Fanout) ForgetOptional(ctx context.Context, scope domain.Scope, factIDs []string) {
	if f == nil || len(factIDs) == 0 || len(f.optional) == 0 {
		return
	}
	f.runOptional(ctx, opForget, func(p port.Projection) error {
		return p.Forget(ctx, scope, factIDs)
	})
}

// RebuildRequired rebuilds required projections from the supplied
// snapshot. Same short-circuit semantics as ProjectRequired.
func (f *Fanout) RebuildRequired(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	if f == nil {
		return nil
	}
	return f.runRequired(ctx, opRebuild, func(p port.Projection) error {
		return p.Rebuild(ctx, scope, facts)
	})
}

// RebuildOptional rebuilds optional projections best-effort.
func (f *Fanout) RebuildOptional(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) {
	if f == nil {
		return
	}
	f.runOptional(ctx, opRebuild, func(p port.Projection) error {
		return p.Rebuild(ctx, scope, facts)
	})
}

// Project is a convenience helper: required then optional in one
// call without compensation. Production write paths should use the
// split ProjectRequired/ProjectOptional pair so they can rollback
// between phases.
func (f *Fanout) Project(ctx context.Context, facts []domain.TemporalFact) error {
	if err := f.ProjectRequired(ctx, facts); err != nil {
		return err
	}
	f.ProjectOptional(ctx, facts)
	return nil
}

// Forget is the unsplit convenience helper.
func (f *Fanout) Forget(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if err := f.ForgetRequired(ctx, scope, factIDs); err != nil {
		return err
	}
	f.ForgetOptional(ctx, scope, factIDs)
	return nil
}

// Rebuild is the unsplit convenience helper.
func (f *Fanout) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
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
func (f *Fanout) Telemetry() port.TelemetryHook {
	if f == nil {
		return telemetry.NopHook{}
	}
	return f.telemetry
}

func (f *Fanout) runRequired(ctx context.Context, op fanoutOp, call func(port.Projection) error) error {
	for _, p := range f.required {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := call(p); err != nil {
			return fmt.Errorf("recall projection %q (%s): %w", p.Name(), op, err)
		}
	}
	return nil
}

// runOptional invokes call on every optional projection. Failures
// never escape the helper: each one is surfaced as a
// Status=Degraded StageDiagnostic carrying the per-projection err so
// observers see exactly which optional projection degraded (Cluster
// C convention). Telemetry is the only emission channel —
// projections do not abort each other.
func (f *Fanout) runOptional(ctx context.Context, op fanoutOp, call func(port.Projection) error) {
	for _, p := range f.optional {
		if err := ctx.Err(); err != nil {
			continue
		}
		started := time.Now()
		err := call(p)
		if err == nil {
			continue
		}
		f.telemetry.OnStage(diagnostic.StageDiagnostic{
			Stage:    fmt.Sprintf("fanout_optional:%s:%s", op, p.Name()),
			Phase:    diagnostic.PhaseWrite,
			StartAt:  started,
			Duration: time.Since(started),
			Status:   diagnostic.StatusDegraded,
			Err:      err.Error(),
			Detail: diagnostic.ProjectDetail{
				Consistency: "optional",
				Results: []diagnostic.ProjectionResult{{
					Name:    p.Name(),
					Latency: time.Since(started),
					Err:     err.Error(),
				}},
			},
		})
	}
}

// ErrProjectionDisabled is returned by helpers that resolve a
// projection by name when the projection has not been registered.
var ErrProjectionDisabled = errdefs.NotFound(errdefs.New("recall projection: not registered"))
