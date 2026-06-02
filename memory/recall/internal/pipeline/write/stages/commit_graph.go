package stages

import (
	"context"
	"errors"
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
	createdObservationIDs, err := s.createdObservationIDs(ctx, state.Scope, delta.Observations)
	if err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, err
	}

	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, fmt.Errorf("recall.Save: graph observations append: %w", err)
	}
	state.GraphObservationIDs = createdObservationIDs
	if s.projection != nil {
		if err := s.projection.ProjectObservations(ctx, delta.Observations); err != nil {
			state.FailedStage = "commit_graph"
			s.cleanupProjectedObservations(ctx, state.Scope, observationIDs(delta.Observations))
			s.cleanupGraphObservations(ctx, state)
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
		s.cleanupProjectedObservations(ctx, state.Scope, observationIDs(delta.Observations))
		s.cleanupGraphObservations(ctx, state)
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

func (s *CommitGraph) cleanupProjectedObservations(ctx context.Context, scope domain.Scope, observationIDs []string) {
	if s == nil || s.projection == nil || len(observationIDs) == 0 {
		return
	}
	_ = s.projection.ForgetObservations(pipeline.DetachCancel(ctx), scope, observationIDs)
}

func (s *CommitGraph) Compensate(ctx context.Context, state *write.WriteState) error {
	if state == nil {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if len(state.GraphLinkIDs) > 0 && s.links != nil {
		_ = s.links.Delete(cleanupCtx, state.Scope, state.GraphLinkIDs)
	}
	projectedObservationIDs := observationIDs(state.GraphDelta.Observations)
	if len(projectedObservationIDs) == 0 {
		projectedObservationIDs = state.GraphObservationIDs
	}
	s.cleanupProjectedObservations(cleanupCtx, state.Scope, projectedObservationIDs)
	s.cleanupGraphObservations(cleanupCtx, state)
	return nil
}

func (s *CommitGraph) createdObservationIDs(ctx context.Context, scope domain.Scope, observations []domain.Observation) ([]string, error) {
	if s == nil || s.observations == nil || len(observations) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(observations))
	for _, observation := range observations {
		if observation.ID == "" {
			continue
		}
		_, err := s.observations.Get(ctx, scope, observation.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, port.ErrNotFound) {
			return nil, fmt.Errorf("recall.Save: graph observation preflight: %w", err)
		}
		out = append(out, observation.ID)
	}
	return out, nil
}

func (s *CommitGraph) cleanupGraphObservations(ctx context.Context, state *write.WriteState) {
	if state == nil || len(state.GraphObservationIDs) == 0 {
		return
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if s.observations != nil {
		_ = s.observations.Delete(cleanupCtx, state.Scope, state.GraphObservationIDs)
	}
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
