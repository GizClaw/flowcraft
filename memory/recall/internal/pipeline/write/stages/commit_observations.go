package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// CommitObservations writes raw Turn observations before assertion extraction.
// This makes Observation the canonical first write in the O/A/L pipeline:
// extractor failures can still be diagnosed, retried, and recalled as raw
// evidence without pretending assertion derivation succeeded.
type CommitObservations struct {
	observations port.ObservationStore
	projection   port.ObservationProjection
}

func NewCommitObservations(observations port.ObservationStore, projection port.ObservationProjection) *CommitObservations {
	return &CommitObservations{observations: observations, projection: projection}
}

func (CommitObservations) Name() string { return "commit_observations" }

func (s *CommitObservations) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if s == nil || s.observations == nil || state == nil || len(state.Turns) == 0 {
		return true, diagnostic.ObservationCommitDetail{}
	}
	return false, nil
}

func (s *CommitObservations) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	now := state.Now
	if now.IsZero() {
		now = started
	}
	delta := graphledger.BuildDelta(state.Scope, nil, nil, turnsFromPort(state.Turns), state.ObservedAt, now, state.SaveOutboxID)
	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		state.FailedStage = "commit_observations"
		return diagnostic.ObservationCommitDetail{
			Observations: len(delta.Observations),
			Latency:      time.Since(started),
		}, fmt.Errorf("recall.Save: graph turn observations append: %w", err)
	}
	if s.projection != nil {
		if err := s.projection.ProjectObservations(ctx, delta.Observations); err != nil {
			state.FailedStage = "commit_observations"
			return diagnostic.ObservationCommitDetail{
				Observations: len(delta.Observations),
				Latency:      time.Since(started),
			}, fmt.Errorf("recall.Save: observation projection project: %w", err)
		}
	}
	state.RawObservationIDs = observationIDs(delta.Observations)
	return diagnostic.ObservationCommitDetail{
		Observations: len(delta.Observations),
		Latency:      time.Since(started),
	}, nil
}

func turnsFromPort(turns []port.TurnContext) []domain.TurnContext {
	if len(turns) == 0 {
		return nil
	}
	out := make([]domain.TurnContext, 0, len(turns))
	for _, turn := range turns {
		out = append(out, domain.TurnContext(turn))
	}
	return out
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*CommitObservations)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*CommitObservations)(nil)
)
