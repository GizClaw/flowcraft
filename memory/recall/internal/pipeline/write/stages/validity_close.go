package stages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

// ValidityClose applies the resolver-issued Closes to the canonical
// store after Append has landed. It tolerates the
// ErrValidityAlreadyClosed sentinel because another writer may have
// already reached the desired post-state — the resolver-issued
// Supersedes pointer on the freshly-appended fact still preserves
// the supersede chain even when the close itself is a no-op.
//
// N:1 supersede support: state.Resolution.Closes already carries one
// ValidityClose per prior fact, so a single successor fact that
// supersedes N priors lands as N entries here. The loop below iterates
// and appends each successful close to state.AppliedCloses;
// Append.Compensate / this stage's Compensate then reopen all N on
// rollback. No special-casing is needed — the slice-based interface
// generalises naturally from 1:1 to 1:N.
//
// The compensator reopens exactly the closes that did land and
// reprojects the prior facts so a downstream project_required
// failure leaves the ledger and projections aligned.
type ValidityClose struct {
	store  port.TemporalStore
	fanout *pipeline.Fanout
	hook   port.TelemetryHook
}

// NewValidityClose constructs the stage. fanout / hook may be nil
// — fanout is only consulted during compensation, hook receives
// Compensated stage diagnostics through OnStage.
func NewValidityClose(store port.TemporalStore, fanout *pipeline.Fanout, hook port.TelemetryHook) *ValidityClose {
	return &ValidityClose{store: store, fanout: fanout, hook: hook}
}

// Name implements pipeline.Stage.
func (ValidityClose) Name() string { return "validity_close" }

// Skip implements pipeline.Conditional.
func (ValidityClose) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if asyncStructuredLegInactive(state) {
		return true, diagnostic.ValidityCloseDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (s *ValidityClose) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	closes := state.Resolution.Closes
	started := time.Now()
	applied := make([]domain.ValidityClose, 0, len(closes))
	benign := 0
	for _, c := range closes {
		err := s.store.UpdateValidity(ctx, c.Scope, c.FactID, c.ValidTo, c.CorrectedBy)
		if err == nil {
			applied = append(applied, c)
			continue
		}
		if errors.Is(err, temporalstore.ErrValidityAlreadyClosed) {
			benign++
			continue
		}
		state.AppliedCloses = applied
		state.FailedStage = "validity_close"
		return diagnostic.ValidityCloseDetail{
			ClosedFacts:  len(applied),
			StoreLatency: time.Since(started),
		}, fmt.Errorf("recall.Save: close superseded: update validity %s: %w", c.FactID, err)
	}
	state.AppliedCloses = applied
	return diagnostic.ValidityCloseDetail{
		ClosedFacts:  len(applied) + benign,
		StoreLatency: time.Since(started),
	}, nil
}

// Compensate implements pipeline.Compensator. It is invoked when a
// stage AFTER validity_close fails (project_required and beyond).
// validity_close's own partial-failure cleanup is handled by
// Append.Compensate via state.FailedStage; see append.go for the
// ordering rationale.
func (s *ValidityClose) Compensate(ctx context.Context, state *write.WriteState) error {
	if len(state.AppliedCloses) == 0 {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	s.reopen(cleanupCtx, state.AppliedCloses)
	s.reproject(cleanupCtx, state.AppliedCloses)
	return nil
}

// reopen restores ValidTo / CorrectedBy on every previously closed fact,
// tolerating ErrNotFound silently. Other errors are swallowed so one failed
// reopen does not stop the remaining best-effort compensation work.
func (s *ValidityClose) reopen(ctx context.Context, closes []domain.ValidityClose) {
	for _, c := range closes {
		err := s.store.ReopenValidity(ctx, c.Scope, c.FactID, c.CorrectedBy)
		if err == nil || errors.Is(err, temporalstore.ErrNotFound) {
			continue
		}
		_ = err
	}
}

// reproject re-runs the required projection fanout on the prior
// facts so projections see the reopened revisions again. Skips any
// fact whose CorrectedBy is still set (someone else legitimately
// re-closed it) and tolerates ErrNotFound silently.
func (s *ValidityClose) reproject(ctx context.Context, closes []domain.ValidityClose) {
	if s.fanout == nil {
		return
	}
	for _, c := range closes {
		fact, err := s.store.Get(ctx, c.Scope, c.FactID)
		if err != nil {
			if errors.Is(err, temporalstore.ErrNotFound) {
				continue
			}
			continue
		}
		if fact.CorrectedBy != "" {
			continue
		}
		_ = s.fanout.ProjectRequired(ctx, []domain.TemporalFact{fact})
	}
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*ValidityClose)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*ValidityClose)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*ValidityClose)(nil)
)
