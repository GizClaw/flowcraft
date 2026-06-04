package stages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	recallingest "github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
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
	if state == nil || (len(state.Resolution.Facts) == 0 && len(state.Resolution.Closes) == 0 && len(state.EpisodeFacts) == 0) {
		return true, diagnostic.GraphCommitDetail{}
	}
	if s == nil || s.observations == nil || s.links == nil {
		if hasParameterFacts(state.Resolution.Facts) {
			return false, nil
		}
		return true, diagnostic.GraphCommitDetail{}
	}
	return false, nil
}

func (s *CommitGraph) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if s == nil || s.observations == nil || s.links == nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.Save: graph dependencies missing for parameter facts")
	}
	if err := s.validateParameterGraphDependencies(ctx, state.Scope, state.Resolution.Facts); err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{Latency: time.Since(started)}, err
	}
	delta := graphledger.BuildDelta(state.Scope, state.Resolution.Facts, state.Resolution.Closes, nil, state.ObservedAt, started, state.SaveOutboxID)
	if err := s.addHistoricalSameObservationLinks(ctx, state.Scope, &delta, state.Resolution.Facts, started); err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, err
	}
	plan, err := s.planObservationCommit(ctx, state.Scope, delta.Observations)
	if err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, err
	}
	delta.Observations = observationsWithIDs(delta.Observations, plan.createdIDs)
	state.GraphDelta = delta.Clone()

	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		state.FailedStage = "commit_graph"
		return diagnostic.GraphCommitDetail{
			Observations: len(delta.Observations),
			Links:        len(delta.Links),
			Latency:      time.Since(started),
		}, fmt.Errorf("recall.Save: graph observations append: %w", err)
	}
	state.GraphObservationIDs = append([]string(nil), plan.createdIDs...)
	if s.projection != nil {
		if err := s.projection.ProjectObservations(ctx, delta.Observations); err != nil {
			state.FailedStage = "commit_graph"
			s.cleanupProjectedObservations(ctx, state.Scope, observationIDs(delta.Observations))
			s.restoreGraphObservations(ctx, state)
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
		s.restoreGraphObservations(ctx, state)
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

func (s *CommitGraph) addHistoricalSameObservationLinks(ctx context.Context, scope domain.Scope, delta *domain.MemoryGraphDelta, facts []domain.TemporalFact, now time.Time) error {
	if s == nil || s.links == nil || delta == nil || len(facts) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(delta.Links))
	for _, link := range delta.Links {
		if link.MergeKey != "" {
			seen[link.MergeKey] = struct{}{}
		}
	}
	add := func(link domain.FactLink) {
		if link.ID == "" || link.MergeKey == "" {
			return
		}
		if _, ok := seen[link.MergeKey]; ok {
			return
		}
		seen[link.MergeKey] = struct{}{}
		delta.Links = append(delta.Links, link)
	}
	for _, fact := range facts {
		if fact.ID == "" {
			continue
		}
		for _, ref := range fact.EvidenceRefs {
			if ref.ObservationID == "" {
				continue
			}
			links, err := s.links.FindByNode(ctx, scope, domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: ref.ObservationID})
			if err != nil {
				return fmt.Errorf("recall.Save: graph same_observation preflight: %w", err)
			}
			for _, existing := range links {
				if existing.Type != domain.LinkSupports || existing.To.Kind != domain.GraphNodeAssertion || existing.To.ID == "" || existing.To.ID == fact.ID {
					continue
				}
				add(graphledger.NewAssertionAssertionLink(scope, domain.LinkSameObservation, fact.ID, existing.To.ID, []string{ref.ObservationID}, []domain.EvidenceRef{ref}, now))
			}
		}
	}
	return nil
}

func hasParameterFacts(facts []domain.TemporalFact) bool {
	for _, fact := range facts {
		if fact.Kind == domain.KindParameter {
			return true
		}
	}
	return false
}

func hasGraphDependencyFacts(facts []domain.TemporalFact) bool {
	for _, fact := range facts {
		if fact.Kind == domain.KindParameter {
			return true
		}
		if fact.Origin.Kind == domain.OriginKindSemanticDerivation {
			for _, ref := range fact.EvidenceRefs {
				if ref.ObservationID != "" || ref.SpanID != "" {
					return true
				}
			}
		}
	}
	return false
}

func (s *CommitGraph) validateParameterGraphDependencies(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	if s == nil {
		return validateParameterGraphDependencies(ctx, nil, nil, scope, facts)
	}
	return validateParameterGraphDependencies(ctx, s.observations, s.links, scope, facts)
}

func validateParameterGraphDependencies(ctx context.Context, observations port.ObservationStore, links port.LinkStore, scope domain.Scope, facts []domain.TemporalFact) error {
	if hasParameterFacts(facts) && (observations == nil || links == nil) {
		return fmt.Errorf("recall.Save: graph_dependencies_missing for parameter facts")
	}
	if !hasParameterFacts(facts) && hasGraphDependencyFacts(facts) && observations == nil {
		return fmt.Errorf("recall.Save: graph_dependencies_missing for canonical evidence facts")
	}
	for _, fact := range facts {
		if len(fact.EvidenceRefs) == 0 {
			if fact.Kind != domain.KindParameter {
				continue
			}
			return fmt.Errorf("recall.Save: parameter fact %q graph_dependencies_missing: no evidence refs", fact.ID)
		}
		supportTexts := make([]string, 0, len(fact.EvidenceRefs))
		for _, ref := range fact.EvidenceRefs {
			if fact.Kind != domain.KindParameter && fact.Origin.Kind != domain.OriginKindSemanticDerivation {
				continue
			}
			if graphledger.IsGeneratedQuoteEvidenceRef(ref) {
				if fact.Kind != domain.KindParameter {
					continue
				}
				return fmt.Errorf("recall.Save: parameter fact %q graph_dependencies_missing: canonical observation/span evidence required", fact.ID)
			}
			if ref.ObservationID == "" || ref.SpanID == "" {
				if fact.Kind != domain.KindParameter {
					continue
				}
				return fmt.Errorf("recall.Save: parameter fact %q graph_dependencies_missing: canonical observation/span evidence required", fact.ID)
			}
			obs, err := observations.Get(ctx, scope, ref.ObservationID)
			if err != nil {
				return fmt.Errorf("recall.Save: fact %q graph_dependencies_missing: observation %q: %w", fact.ID, ref.ObservationID, err)
			}
			if !recallingest.ExtractableEvidenceWindowObservation(obs) {
				return fmt.Errorf("recall.Save: fact %q graph_dependencies_missing: observation %q is not extractable raw evidence", fact.ID, ref.ObservationID)
			}
			span, ok := findValidObservationSpan(obs, ref.SpanID)
			if !ok {
				return fmt.Errorf("recall.Save: fact %q graph_dependencies_missing: span %q not found on observation %q", fact.ID, ref.SpanID, ref.ObservationID)
			}
			supportTexts = append(supportTexts, span.Text)
		}
		if fact.Kind == domain.KindParameter {
			if len(supportTexts) == 0 {
				return fmt.Errorf("recall.Save: parameter fact %q graph_dependencies_missing: canonical observation/span evidence required", fact.ID)
			}
			if err := recallingest.ValidateParameterFactEvidenceSupport(fact, supportTexts); err != nil {
				return fmt.Errorf("recall.Save: parameter fact %q graph_dependencies_unsupported: %w", fact.ID, err)
			}
			continue
		}
	}
	return nil
}

func observationHasValidSpan(obs domain.Observation, spanID string) bool {
	_, ok := findValidObservationSpan(obs, spanID)
	return ok
}

func findValidObservationSpan(obs domain.Observation, spanID string) (domain.SourceEvidenceSpan, bool) {
	spans, err := recallingest.SourceEvidenceSpansFromObservation(obs)
	if err != nil {
		return domain.SourceEvidenceSpan{}, false
	}
	for _, span := range spans {
		if span.SpanID == spanID {
			return span, true
		}
	}
	return domain.SourceEvidenceSpan{}, false
}

func observationHasSpan(obs domain.Observation, spanID string) bool {
	for _, span := range obs.Spans {
		if span.ID == spanID && span.ObservationID == obs.ID {
			return true
		}
	}
	return false
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
	s.restoreGraphObservations(cleanupCtx, state)
	return nil
}

type observationCommitPlan struct {
	createdIDs []string
}

func (s *CommitGraph) planObservationCommit(ctx context.Context, scope domain.Scope, observations []domain.Observation) (observationCommitPlan, error) {
	if s == nil || s.observations == nil || len(observations) == 0 {
		return observationCommitPlan{}, nil
	}
	var out observationCommitPlan
	seen := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		if observation.ID == "" {
			continue
		}
		if _, ok := seen[observation.ID]; ok {
			continue
		}
		seen[observation.ID] = struct{}{}
		_, err := s.observations.Get(ctx, scope, observation.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, port.ErrNotFound) {
			return observationCommitPlan{}, fmt.Errorf("recall.Save: graph observation preflight: %w", err)
		}
		out.createdIDs = append(out.createdIDs, observation.ID)
	}
	return out, nil
}

func (s *CommitGraph) restoreGraphObservations(ctx context.Context, state *write.WriteState) {
	if state == nil || s.observations == nil {
		return
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if len(state.GraphObservationIDs) > 0 {
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

func observationsWithIDs(observations []domain.Observation, ids []string) []domain.Observation {
	if len(observations) == 0 || len(ids) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			keep[id] = struct{}{}
		}
	}
	out := make([]domain.Observation, 0, len(keep))
	seen := make(map[string]struct{}, len(keep))
	for _, observation := range observations {
		if _, ok := keep[observation.ID]; !ok {
			continue
		}
		if _, dup := seen[observation.ID]; dup {
			continue
		}
		seen[observation.ID] = struct{}{}
		out = append(out, observation.Clone())
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
