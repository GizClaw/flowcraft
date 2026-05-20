package stages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// Append writes the resolved facts to the canonical store. The
// compensator deletes whatever was appended on any downstream
// failure. When the downstream failure is validity_close, the
// compensator also reopens the validity-close prefix that did land
// — preserving the legacy rollbackAppendedFacts ordering (store
// delete first, then reopen) which the v2 framework cannot reproduce
// from validity_close's compensator alone because the framework does
// not invoke a failing stage's own compensator.
type Append struct {
	store port.TemporalStore
	hook  port.TelemetryHook
}

// NewAppend constructs an Append stage. hook may be nil — the
// compensator only emits OnProjection events through it when a
// downstream stage fails AND store cleanup itself errors.
func NewAppend(store port.TemporalStore, hook port.TelemetryHook) *Append {
	return &Append{store: store, hook: hook}
}

// Name implements pipeline.Stage.
func (Append) Name() string { return "append" }

// Run implements pipeline.Stage.
func (s *Append) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if err := s.store.Append(ctx, state.Resolution.Facts); err != nil {
		state.FailedStage = "append"
		return diagnostic.AppendDetail{
			Facts:        len(state.Resolution.Facts),
			StoreLatency: time.Since(started),
		}, fmt.Errorf("recall.Save: store append: %w", err)
	}
	ids := make([]string, len(state.Resolution.Facts))
	for i, f := range state.Resolution.Facts {
		ids[i] = f.ID
	}
	state.AppendedFactIDs = ids
	if state.Trace != nil {
		state.Trace.Appended = append([]domain.TemporalFact(nil), state.Resolution.Facts...)
	}
	return diagnostic.AppendDetail{
		Facts:        len(state.Resolution.Facts),
		StoreLatency: time.Since(started),
	}, nil
}

// Compensate implements pipeline.Compensator. The emit name is
// chosen to mirror the legacy rollback helpers byte-for-byte:
//
//   - state.FailedStage == "validity_close" → rollbackAppendedFacts
//     fired "save_rollback.appended_facts" before reopening the
//     applied close prefix. We honour the same name AND replay the
//     reopen here so the framework's per-stage compensator order
//     does not invert the legacy {delete, reopen} sequence.
//   - any other downstream failure (project_required and beyond) →
//     rollbackSave fired "save_rollback.store_delete"; the reopen
//     and reproject moves happen in validity_close's own
//     compensator (the framework still invokes it because
//     validity_close ran to completion).
func (s *Append) Compensate(ctx context.Context, state *write.WriteState) error {
	if len(state.AppendedFactIDs) == 0 {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	failedAtValidityClose := state.FailedStage == "validity_close"
	rollbackName := "save_rollback.store_delete"
	if failedAtValidityClose {
		rollbackName = "save_rollback.appended_facts"
	}
	if err := s.store.Delete(cleanupCtx, state.Scope, state.AppendedFactIDs); err != nil {
		s.emit(port.ProjectionEvent{
			Projection:  rollbackName,
			Op:          port.OpForget,
			Consistency: projection.Required.String(),
			FactCount:   len(state.AppendedFactIDs),
			Err:         fmt.Errorf("rollback cleanup: %w", err),
		})
	}
	if failedAtValidityClose {
		s.reopenAppliedCloses(cleanupCtx, state.AppliedCloses)
	}
	return nil
}

// reopenAppliedCloses mirrors the legacy reopenAfterRollback helper:
// for every close that did land before validity_close failed, undo
// it via Store.ReopenValidity. ErrNotFound is tolerated silently
// (the prior fact may already have been forgotten). ErrReopenConflict
// (and any other surface) reports via telemetry but never aborts the
// remaining reopens.
func (s *Append) reopenAppliedCloses(ctx context.Context, closes []domain.ValidityClose) {
	for _, c := range closes {
		err := s.store.ReopenValidity(ctx, c.Scope, c.FactID, c.CorrectedBy)
		if err == nil || errors.Is(err, temporalstore.ErrNotFound) {
			continue
		}
		s.emit(port.ProjectionEvent{
			Projection:  "save_rollback.reopen_validity",
			Op:          port.OpProject,
			Consistency: projection.Required.String(),
			FactCount:   1,
			Err:         fmt.Errorf("reopen %s: %w", c.FactID, err),
		})
	}
}

// emit routes the legacy ProjectionEvent through the hook stored at
// construction. A nil hook is silently ignored — the new OnStage
// rail still fires through the framework.
func (s *Append) emit(ev port.ProjectionEvent) {
	if s.hook == nil {
		return
	}
	s.hook.OnProjection(ev)
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*Append)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*Append)(nil)
)
