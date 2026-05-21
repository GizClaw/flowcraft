// Package stages owns the apply_feedback stage that powers
// Memory.Reinforce / Memory.Penalize (Cluster A 2026-05-21). The
// stage performs the canonical UpdateFeedback write and the
// single-fact reproject (Cluster D — keeps retrieval Doc metadata
// fresh) so a single StageDiagnostic captures both effects.
package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/feedback"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ApplyFeedback is the single stage of the feedback pipeline.
//
// Run order:
//
//  1. Validate FactID + at least one positive delta.
//  2. store.UpdateFeedback applies the deltas (clamped non-negative).
//  3. store.Get refreshes the canonical snapshot so the projection
//     sees the post-clamp Reinforcement / Penalty.
//  4. fanout.ProjectRequired reprojects the single fact — this is
//     the Cluster D fix: retrieval Doc metadata (MetaReinforcement /
//     MetaPenalty) tracks the canonical fact instead of drifting
//     until the next full Save.
//
// fanout.ProjectOptional runs best-effort right after; optional
// projection failures only emit telemetry (matching the write
// pipeline's project_optional stage).
//
// When projection fails after UpdateFeedback, the stage reverses
// the deltas on the store so caller retries stay idempotent.
type ApplyFeedback struct {
	store  port.TemporalStore
	fanout *pipeline.Fanout
}

// NewApplyFeedback constructs the stage.
func NewApplyFeedback(store port.TemporalStore, fanout *pipeline.Fanout) *ApplyFeedback {
	return &ApplyFeedback{store: store, fanout: fanout}
}

// Name implements pipeline.Stage.
func (ApplyFeedback) Name() string { return "apply_feedback" }

// Run implements pipeline.Stage.
func (s *ApplyFeedback) Run(ctx context.Context, state *feedback.State) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.FeedbackDetail{
		FactID:             state.FactID,
		ReinforcementDelta: state.ReinforcementDelta,
		PenaltyDelta:       state.PenaltyDelta,
	}

	if state.FactID == "" {
		detail.Latency = time.Since(started)
		return detail, errdefs.Validationf("recall.Feedback: fact id is required")
	}
	if state.ReinforcementDelta <= 0 && state.PenaltyDelta <= 0 {
		detail.Latency = time.Since(started)
		return detail, errdefs.Validationf("recall.Feedback: at least one delta must be positive")
	}
	if state.ReinforcementDelta < 0 || state.PenaltyDelta < 0 {
		detail.Latency = time.Since(started)
		return detail, errdefs.Validationf("recall.Feedback: deltas must be non-negative")
	}

	existing, err := s.store.Get(ctx, state.Scope, state.FactID)
	if err != nil {
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Feedback: get: %w", err)
	}
	if existing.Kind == domain.KindEpisode {
		detail.Latency = time.Since(started)
		return detail, errdefs.Validationf("recall.Feedback: KindEpisode facts cannot receive reinforcement or penalty")
	}

	if err := s.store.UpdateFeedback(ctx, state.Scope, state.FactID, state.ReinforcementDelta, state.PenaltyDelta); err != nil {
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Feedback: update: %w", err)
	}

	updated, err := s.store.Get(ctx, state.Scope, state.FactID)
	if err != nil {
		_ = s.store.UpdateFeedback(ctx, state.Scope, state.FactID, -state.ReinforcementDelta, -state.PenaltyDelta)
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Feedback: re-get: %w", err)
	}
	state.Updated = updated

	if s.fanout != nil {
		if err := s.fanout.ProjectRequired(ctx, []domain.TemporalFact{updated}); err != nil {
			_ = s.store.UpdateFeedback(ctx, state.Scope, state.FactID, -state.ReinforcementDelta, -state.PenaltyDelta)
			detail.Latency = time.Since(started)
			return detail, err
		}
		s.fanout.ProjectOptional(ctx, []domain.TemporalFact{updated})
	}

	detail.Latency = time.Since(started)
	return detail, nil
}

var _ pipeline.Stage[*feedback.State] = (*ApplyFeedback)(nil)
