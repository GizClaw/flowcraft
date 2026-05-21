// Package stages owns the single forget_all stage that powers
// Memory.ForgetAll (Phase D.8 C9). It lives here rather than inline
// in the facade so the operation honours framework principle #2
// "Stages over Procedural" and #6 "Stage is the one source of
// diagnostics".
package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/forget"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ErrScopeKeyMismatch is returned when Mode == Hard and the caller-
// supplied ConfirmScopeKey does not equal scope.CanonicalKey(). The
// guard is the last line of defence against accidental cross-tenant
// nuke: callers who copy the scope by value can still copy the key,
// so we require the freshly-computed canonical key as confirmation.
// The error is sentinel-stable so callers can errors.Is against it.
var ErrScopeKeyMismatch = errdefs.Forbidden(errdefs.New("recall.ForgetAll: confirmScopeKey mismatch"))

// ForgetAll implements the GDPR Art.17 / CCPA 1798.105 "delete me"
// operation. It is intentionally a single stage:
//
//   - the operation is atomic from the caller's point of view (one
//     scope, one outcome) so splitting into list / mark_closed /
//     clear / delete would expose intermediate states with no
//     independent observability value;
//   - one ForgetAllDetail carries every counter operators want
//     (Deleted / ProjectionsCleared / EvidenceCleared) without
//     stitching across multiple StageDiagnostic records;
//   - failure semantics are simple — any subsystem failure aborts
//     the whole operation; we deliberately do not run partial
//     compensation here (a half-deleted scope is exactly what the
//     caller asked for under Hard mode, just with fewer rows than
//     they hoped).
type ForgetAll struct {
	store          port.TemporalStore
	fanout         *pipeline.Fanout
	projections    []port.Projection
	evidenceLookup port.EvidenceStore
}

// NewForgetAll constructs a forget_all stage. fanout drives the
// projection ClearScope dispatch; projections is the registered set
// so the stage can count how many were cleared; evidenceLookup is
// optional and used only by Hard mode to count cleared rows for
// telemetry (the projection layer already handles the actual
// deletion via ClearScope on the evidence projection).
func NewForgetAll(
	store port.TemporalStore,
	fanout *pipeline.Fanout,
	projections []port.Projection,
	evidenceLookup port.EvidenceStore,
) *ForgetAll {
	return &ForgetAll{
		store:          store,
		fanout:         fanout,
		projections:    projections,
		evidenceLookup: evidenceLookup,
	}
}

// Name implements pipeline.Stage.
func (ForgetAll) Name() string { return "forget_all" }

// Run implements pipeline.Stage.
//
//nolint:gocyclo // five branches map 1:1 to the two-mode flow; splitting hides intent.
func (s *ForgetAll) Run(ctx context.Context, state *forget.State) (diagnostic.StageDetail, error) {
	started := time.Now()
	scopeKey := state.Scope.CanonicalKey()
	mode := domain.NormalizeForgetMode(state.Mode)

	if mode == domain.ForgetHard && state.ConfirmScopeKey != scopeKey {
		return diagnostic.ForgetAllDetail{ScopeKey: scopeKey, Mode: string(mode)},
			ErrScopeKeyMismatch
	}

	facts, err := s.store.List(ctx, state.Scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return diagnostic.ForgetAllDetail{ScopeKey: scopeKey, Mode: string(mode)},
			fmt.Errorf("recall.ForgetAll: list: %w", err)
	}
	if len(facts) == 0 {
		return diagnostic.ForgetAllDetail{
			ScopeKey: scopeKey,
			Mode:     string(mode),
			Latency:  time.Since(started),
		}, nil
	}

	switch mode {
	case domain.ForgetSoft:
		return s.runSoft(ctx, state, scopeKey, facts, started)
	default:
		return s.runHard(ctx, state, scopeKey, facts, started)
	}
}

// runSoft marks every fact Closed=true and re-projects the closed
// view so downstream sources (read pipeline) hide them by default.
// Store rows and evidence are preserved — Memory.History must still
// walk the supersede chain after a Soft ForgetAll.
func (s *ForgetAll) runSoft(
	ctx context.Context,
	state *forget.State,
	scopeKey string,
	facts []domain.TemporalFact,
	started time.Time,
) (diagnostic.StageDetail, error) {
	for _, f := range facts {
		if err := s.store.MarkClosed(ctx, state.Scope, f.ID, true); err != nil {
			return diagnostic.ForgetAllDetail{
				ScopeKey: scopeKey,
				Mode:     string(domain.ForgetSoft),
				Latency:  time.Since(started),
			}, fmt.Errorf("recall.ForgetAll: mark closed: %w", err)
		}
	}
	closed := make([]domain.TemporalFact, len(facts))
	for i, f := range facts {
		f.Closed = true
		closed[i] = f
	}
	if err := s.fanout.ProjectRequired(ctx, closed); err != nil {
		return diagnostic.ForgetAllDetail{
			ScopeKey: scopeKey,
			Mode:     string(domain.ForgetSoft),
			Deleted:  len(facts),
			Latency:  time.Since(started),
		}, err
	}
	s.fanout.ProjectOptional(ctx, closed)
	state.Deleted = len(facts)
	return diagnostic.ForgetAllDetail{
		ScopeKey: scopeKey,
		Mode:     string(domain.ForgetSoft),
		Deleted:  len(facts),
		// Soft mode does NOT invoke ClearScope — projections rewrite
		// the closed rows in place. EvidenceCleared stays 0 (audit
		// preservation).
		ProjectionsCleared: 0,
		EvidenceCleared:    0,
		Latency:            time.Since(started),
	}, nil
}

// runHard fans ClearScope across every registered projection, then
// physically deletes the canonical store partition. Evidence-cleared
// count is read from the evidence store (if present) BEFORE the
// projection ClearScope so the count reflects what was actually
// removed, not an after-the-fact zero.
func (s *ForgetAll) runHard(
	ctx context.Context,
	state *forget.State,
	scopeKey string,
	facts []domain.TemporalFact,
	started time.Time,
) (diagnostic.StageDetail, error) {
	// Snapshot evidence count first — once ClearScope wipes the
	// evidence projection there is no way to recount.
	evidenceCount := 0
	if s.evidenceLookup != nil {
		if ids, err := s.evidenceLookup.ListFactIDs(ctx, state.Scope); err == nil {
			evidenceCount = len(ids)
		}
	}

	cleared, err := s.clearAllProjections(ctx, state.Scope)
	if err != nil {
		return diagnostic.ForgetAllDetail{
			ScopeKey:           scopeKey,
			Mode:               string(domain.ForgetHard),
			ProjectionsCleared: cleared,
			Latency:            time.Since(started),
		}, err
	}

	n, err := s.store.DeleteByScope(ctx, state.Scope)
	if err != nil {
		return diagnostic.ForgetAllDetail{
			ScopeKey:           scopeKey,
			Mode:               string(domain.ForgetHard),
			ProjectionsCleared: cleared,
			EvidenceCleared:    evidenceCount,
			Latency:            time.Since(started),
		}, fmt.Errorf("recall.ForgetAll: store delete: %w", err)
	}
	if n == 0 {
		// Some stores return a count, others 0 on success; fall
		// back to the snapshot we have so the detail is non-zero
		// when work was actually done.
		n = len(facts)
	}
	state.Deleted = n
	return diagnostic.ForgetAllDetail{
		ScopeKey:           scopeKey,
		Mode:               string(domain.ForgetHard),
		Deleted:            n,
		ProjectionsCleared: cleared,
		EvidenceCleared:    evidenceCount,
		Latency:            time.Since(started),
	}, nil
}

// clearAllProjections invokes ClearScope on every registered
// projection — Required first, then Optional — matching the fanout
// order. We deliberately do NOT short-circuit on Optional errors
// (best-effort), but Required failures abort. The returned count is
// the number of projections whose ClearScope returned nil.
func (s *ForgetAll) clearAllProjections(ctx context.Context, scope domain.Scope) (int, error) {
	cleared := 0
	for _, p := range s.projections {
		if p == nil {
			continue
		}
		err := p.ClearScope(ctx, scope)
		if err != nil {
			if p.Consistency() == port.Required {
				return cleared, fmt.Errorf("recall.ForgetAll: projection %q clear: %w", p.Name(), err)
			}
			continue
		}
		cleared++
	}
	return cleared, nil
}

var _ pipeline.Stage[*forget.State] = (*ForgetAll)(nil)
