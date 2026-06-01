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

// CommitGraph writes the experimental Observation/Assertion/Link ledger rows
// derived from the current Save. It is intentionally downstream of assertion
// append + validity close so links only reference successfully committed facts.
type CommitGraph struct {
	observations port.ObservationStore
	links        port.LinkStore
	projection   port.ObservationProjection
}

// NewCommitGraph constructs a graph commit stage. Nil stores disable the stage.
func NewCommitGraph(observations port.ObservationStore, links port.LinkStore, projection port.ObservationProjection) *CommitGraph {
	return &CommitGraph{observations: observations, links: links, projection: projection}
}

func (CommitGraph) Name() string { return "commit_graph" }

func (s *CommitGraph) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if s.observations == nil || s.links == nil {
		return true, diagnostic.GraphCommitDetail{}
	}
	if state == nil || (len(state.Resolution.Facts) == 0 && len(state.Resolution.Closes) == 0 && len(state.EpisodeFacts) == 0) {
		return true, diagnostic.GraphCommitDetail{}
	}
	return false, nil
}

func (s *CommitGraph) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	delta := graphledger.BuildDelta(state.Scope, state.Resolution.Facts, state.Resolution.Closes, nil, state.ObservedAt, started, state.SaveOutboxID)
	state.GraphDelta = delta.Clone()

	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, fmt.Errorf("recall.Save: graph observations append: %w", err)
	}
	state.GraphObservationIDs = observationIDs(delta.Observations)
	if s.projection != nil {
		if err := s.projection.ProjectObservations(ctx, delta.Observations); err != nil {
			state.FailedStage = "commit_graph"
			_ = s.observations.Delete(pipeline.DetachCancel(ctx), state.Scope, state.GraphObservationIDs)
			state.GraphObservationIDs = nil
			return diagnostic.GraphCommitDetail{
				Observations: len(delta.Observations),
				Links:        len(delta.Links),
				Latency:      time.Since(started),
			}, fmt.Errorf("recall.Save: graph observation projection: %w", err)
		}
	}

	if err := s.links.Append(ctx, delta.Links); err != nil {
		state.FailedStage = "commit_graph"
		_ = s.observations.Delete(pipeline.DetachCancel(ctx), state.Scope, state.GraphObservationIDs)
		state.GraphObservationIDs = nil
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, fmt.Errorf("recall.Save: graph links append: %w", err)
	}
	state.GraphLinkIDs = linkIDs(delta.Links)

	return diagnostic.GraphCommitDetail{
		Observations: len(delta.Observations),
		Links:        len(delta.Links),
		Latency:      time.Since(started),
	}, nil
}

func (s *CommitGraph) Compensate(ctx context.Context, state *write.WriteState) error {
	if state == nil {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if len(state.GraphLinkIDs) > 0 && s.links != nil {
		_ = s.links.Delete(cleanupCtx, state.Scope, state.GraphLinkIDs)
	}
	if len(state.GraphObservationIDs) > 0 && s.observations != nil {
		_ = s.observations.Delete(cleanupCtx, state.Scope, state.GraphObservationIDs)
	}
	return nil
}

func observationIDs(observations []domain.Observation) []string {
	out := make([]string, 0, len(observations))
	for _, o := range observations {
		if o.ID != "" {
			out = append(out, o.ID)
		}
	}
	return out
}

func linkIDs(links []domain.FactLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		if l.ID != "" {
			out = append(out, l.ID)
		}
	}
	return out
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*CommitGraph)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*CommitGraph)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*CommitGraph)(nil)
)
