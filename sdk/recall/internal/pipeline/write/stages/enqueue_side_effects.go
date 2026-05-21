package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// EnqueueSideEffects records commit-after work in the durable side-effect
// outbox while the scope write lock is held. Projection, evolution, and
// embedding run later via SideEffectProcessor outside the lock.
type EnqueueSideEffects struct {
	outbox         port.SideEffectOutbox
	needsEmbedding bool
	evolution      port.EvolutionRunner
}

// NewEnqueueSideEffects constructs the stage.
func NewEnqueueSideEffects(outbox port.SideEffectOutbox, needsEmbedding bool, evolution port.EvolutionRunner) *EnqueueSideEffects {
	return &EnqueueSideEffects{outbox: outbox, needsEmbedding: needsEmbedding, evolution: evolution}
}

// Name implements pipeline.Stage.
func (EnqueueSideEffects) Name() string { return "enqueue_side_effects" }

// Run implements pipeline.Stage.
func (s *EnqueueSideEffects) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if s.outbox == nil {
		state.FailedStage = "enqueue_side_effects"
		return diagnostic.EnqueueSideEffectsDetail{SaveOutboxID: state.SaveOutboxID},
			fmt.Errorf("recall.Save: side-effect outbox not configured")
	}
	detail := diagnostic.EnqueueSideEffectsDetail{SaveOutboxID: state.SaveOutboxID}
	batchID := state.SaveOutboxID

	enqueue := func(kind port.SideEffectJobKind, facts []domain.TemporalFact) error {
		if len(facts) == 0 {
			return nil
		}
		detail.Enqueued++
		return s.outbox.Enqueue(ctx, port.SideEffectJob{
			RequestID: batchID,
			Scope:     state.Scope,
			Kind:      kind,
			Facts:     facts,
		})
	}

	if len(state.EpisodeFacts) > 0 {
		if err := enqueue(port.SideEffectProjectEpisodeEvidence, state.EpisodeFacts); err != nil {
			state.FailedStage = "enqueue_side_effects"
			return detail, err
		}
	}

	if !asyncStructuredLegInactive(state) && state.HasWork() {
		facts := state.Resolution.Facts
		if err := enqueue(port.SideEffectProjectRequired, facts); err != nil {
			state.FailedStage = "enqueue_side_effects"
			return detail, err
		}
		if err := enqueue(port.SideEffectProjectOptional, facts); err != nil {
			state.FailedStage = "enqueue_side_effects"
			return detail, err
		}
		if s.needsEmbedding {
			if err := enqueue(port.SideEffectEmbeddingBackfill, facts); err != nil {
				state.FailedStage = "enqueue_side_effects"
				return detail, err
			}
		}
	}

	if s.evolution != nil && len(state.AppendedFactIDs) > 0 {
		evFacts := evolutionFacts(state)
		if len(evFacts) > 0 {
			if err := enqueue(port.SideEffectEvolutionAfterSave, evFacts); err != nil {
				state.FailedStage = "enqueue_side_effects"
				return detail, err
			}
		}
	}

	detail.Latency = time.Since(started)
	state.SideEffectsEnqueued = detail.Enqueued
	return detail, nil
}

func evolutionFacts(state *write.WriteState) []domain.TemporalFact {
	if state == nil {
		return nil
	}
	if len(state.Resolution.Facts) > 0 {
		out := make([]domain.TemporalFact, len(state.Resolution.Facts))
		for i, f := range state.Resolution.Facts {
			out[i] = f.Clone()
		}
		return out
	}
	out := make([]domain.TemporalFact, 0, len(state.AppendedFactIDs))
	for _, id := range state.AppendedFactIDs {
		out = append(out, domain.TemporalFact{ID: id, Scope: state.Scope})
	}
	return out
}

// Compensate cancels every outbox job from this Save batch.
func (s *EnqueueSideEffects) Compensate(ctx context.Context, state *write.WriteState) error {
	if s.outbox == nil || state == nil || state.SaveOutboxID == "" {
		return nil
	}
	return s.outbox.Cancel(pipeline.DetachCancel(ctx), state.SaveOutboxID)
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*EnqueueSideEffects)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*EnqueueSideEffects)(nil)
)
