package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	recallingest "github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// GraphLedger rebuilds the experimental canonical graph from TemporalFacts.
// It only runs during full rebuilds; single projection rebuilds intentionally
// leave canonical graph stores untouched.
type GraphLedger struct {
	observations port.ObservationStore
	links        port.LinkStore
	projection   port.ObservationProjection
}

func NewGraphLedger(observations port.ObservationStore, links port.LinkStore, projection port.ObservationProjection) *GraphLedger {
	return &GraphLedger{observations: observations, links: links, projection: projection}
}

func (GraphLedger) Name() string { return "graph_ledger" }

func (s *GraphLedger) Skip(_ context.Context, state *rebuild.RebuildState) (bool, diagnostic.StageDetail) {
	if s == nil || s.observations == nil || s.links == nil {
		return true, diagnostic.RebuildGraphDetail{}
	}
	if state == nil || state.ProjectionFilter != "" {
		return true, diagnostic.RebuildGraphDetail{}
	}
	return false, nil
}

func (s *GraphLedger) Run(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	started := time.Now()
	rawObservations, err := s.observations.List(ctx, state.Scope, port.ObservationListQuery{Kinds: []domain.ObservationKind{
		domain.ObservationKindTurn,
		domain.ObservationKindDocument,
	}})
	if err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph raw observations list: %w", err)
	}
	if err := validateRebuildParameterEvidence(state.Facts, rawObservations); err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)}, err
	}
	delta := graphledger.BuildDelta(state.Scope, state.Facts, nil, nil, time.Time{}, started, "")
	priorObservations, err := s.observations.List(ctx, state.Scope, port.ObservationListQuery{})
	if err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph observations snapshot: %w", err)
	}
	priorLinks, err := s.links.List(ctx, state.Scope, port.LinkListQuery{})
	if err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph links snapshot: %w", err)
	}

	if _, err := s.links.DeleteByScope(ctx, state.Scope); err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph links clear: %w", err)
	}
	if _, err := s.observations.DeleteByScope(ctx, state.Scope); err != nil {
		err = s.restorePriorLedger(ctx, state.Scope, priorObservations, priorLinks, err)
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph observations clear: %w", err)
	}
	if len(rawObservations) > 0 {
		delta.Observations = mergeRebuildObservations(delta.Observations, rawObservations)
	}
	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		err = s.restorePriorLedger(ctx, state.Scope, priorObservations, priorLinks, err)
		return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph observations append: %w", err)
	}
	if s.projection != nil {
		if err := s.projection.RebuildObservations(ctx, state.Scope, delta.Observations); err != nil {
			err = s.restorePriorLedger(ctx, state.Scope, priorObservations, priorLinks, err)
			return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
				fmt.Errorf("recall.RebuildAll: observation projection rebuild: %w", err)
		}
	}
	if err := s.links.Append(ctx, delta.Links); err != nil {
		err = s.restorePriorLedger(ctx, state.Scope, priorObservations, priorLinks, err)
		return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph links append: %w", err)
	}

	return diagnostic.RebuildGraphDetail{
		Observations: len(delta.Observations),
		Links:        len(delta.Links),
		Latency:      time.Since(started),
	}, nil
}

func validateRebuildParameterEvidence(facts []domain.TemporalFact, observations []domain.Observation) error {
	byID := make(map[string]domain.Observation, len(observations))
	for _, obs := range observations {
		if obs.ID != "" {
			byID[obs.ID] = obs
		}
	}
	for _, fact := range facts {
		if fact.Kind != domain.KindParameter {
			continue
		}
		if len(fact.EvidenceRefs) == 0 {
			return fmt.Errorf("recall.RebuildAll: parameter fact %q graph_dependencies_missing: no evidence refs", fact.ID)
		}
		supportTexts := make([]string, 0, len(fact.EvidenceRefs))
		for _, ref := range fact.EvidenceRefs {
			obs, ok := byID[ref.ObservationID]
			if !ok {
				return fmt.Errorf("recall.RebuildAll: parameter fact %q graph_dependencies_missing: observation %q", fact.ID, ref.ObservationID)
			}
			if !recallingest.ExtractableEvidenceWindowObservation(obs) {
				return fmt.Errorf("recall.RebuildAll: parameter fact %q graph_dependencies_missing: observation %q is not extractable raw evidence", fact.ID, ref.ObservationID)
			}
			span, ok := rebuildValidObservationSpan(obs, ref.SpanID)
			if !ok {
				return fmt.Errorf("recall.RebuildAll: parameter fact %q graph_dependencies_missing: span %q not found on observation %q", fact.ID, ref.SpanID, ref.ObservationID)
			}
			supportTexts = append(supportTexts, span.Text)
		}
		if err := recallingest.ValidateParameterFactEvidenceSupport(fact, supportTexts); err != nil {
			return fmt.Errorf("recall.RebuildAll: parameter fact %q graph_dependencies_unsupported: %w", fact.ID, err)
		}
	}
	return nil
}

func rebuildObservationHasValidSpan(obs domain.Observation, spanID string) bool {
	_, ok := rebuildValidObservationSpan(obs, spanID)
	return ok
}

func rebuildValidObservationSpan(obs domain.Observation, spanID string) (domain.SourceEvidenceSpan, bool) {
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

func (s *GraphLedger) restorePriorLedger(ctx context.Context, scope domain.Scope, observations []domain.Observation, links []domain.FactLink, cause error) error {
	if s == nil {
		return cause
	}
	var restoreErr error
	if s.links != nil {
		if _, err := s.links.DeleteByScope(ctx, scope); err != nil {
			restoreErr = err
		}
		if len(links) > 0 {
			if err := s.links.Append(ctx, links); err != nil && restoreErr == nil {
				restoreErr = err
			}
		}
	}
	if s.observations != nil {
		if _, err := s.observations.DeleteByScope(ctx, scope); err != nil && restoreErr == nil {
			restoreErr = err
		}
		if len(observations) > 0 {
			if err := s.observations.Append(ctx, observations); err != nil && restoreErr == nil {
				restoreErr = err
			}
		}
	}
	if s.projection != nil {
		if err := s.projection.RebuildObservations(ctx, scope, observations); err != nil && restoreErr == nil {
			restoreErr = err
		}
	}
	if restoreErr != nil {
		return fmt.Errorf("%w; restore prior graph ledger: %v", cause, restoreErr)
	}
	return cause
}

func mergeRebuildObservations(existing, rawTurns []domain.Observation) []domain.Observation {
	out := make([]domain.Observation, 0, len(existing)+len(rawTurns))
	byID := make(map[string]int, len(existing)+len(rawTurns))
	for _, observation := range existing {
		if observation.ID == "" {
			continue
		}
		byID[observation.ID] = len(out)
		out = append(out, observation.Clone())
	}
	for _, observation := range rawTurns {
		if observation.ID == "" {
			continue
		}
		if idx, ok := byID[observation.ID]; ok {
			merged, _, conflict := domain.MergeObservation(out[idx], observation)
			if !conflict {
				out[idx] = merged
			}
			continue
		}
		byID[observation.ID] = len(out)
		out = append(out, observation.Clone())
	}
	return out
}

var (
	_ pipeline.Stage[*rebuild.RebuildState]       = (*GraphLedger)(nil)
	_ pipeline.Conditional[*rebuild.RebuildState] = (*GraphLedger)(nil)
)
