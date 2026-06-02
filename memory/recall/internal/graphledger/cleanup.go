package graphledger

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// ClearAssertion removes all graph links touching an assertion and deletes
// orphaned observations that were only reachable through those links.
func ClearAssertion(ctx context.Context, scope domain.Scope, factID string, observations port.ObservationStore, links port.LinkStore) (int, int, []string, error) {
	if links == nil || factID == "" {
		return 0, 0, nil, nil
	}
	node := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	deletedObservationIDs, err := PlanClearAssertion(ctx, scope, factID, observations, links)
	if err != nil {
		return 0, 0, nil, err
	}
	linkCount, err := links.DeleteByNode(ctx, scope, node)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("graph ledger: delete assertion links: %w", err)
	}
	if observations == nil || len(deletedObservationIDs) == 0 {
		return linkCount, 0, nil, nil
	}
	var deletedObservations int
	for _, observationID := range deletedObservationIDs {
		if err := observations.Delete(ctx, scope, []string{observationID}); err != nil {
			return linkCount, deletedObservations, deletedObservationIDs, fmt.Errorf("graph ledger: delete observation: %w", err)
		}
		deletedObservations++
	}
	return linkCount, deletedObservations, deletedObservationIDs, nil
}

// PlanClearAssertion returns the observations that ClearAssertion would delete
// after removing the assertion's links. It is read-only so callers can clean
// derived projections before mutating the graph ledger.
func PlanClearAssertion(ctx context.Context, scope domain.Scope, factID string, observations port.ObservationStore, links port.LinkStore) ([]string, error) {
	if links == nil || observations == nil || factID == "" {
		return nil, nil
	}
	node := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	existing, err := links.FindByNode(ctx, scope, node)
	if err != nil {
		return nil, fmt.Errorf("graph ledger: find assertion links: %w", err)
	}
	observationIDs, spanIDsByObservation := LinkedObservationAndSpanIDs(existing, node)
	if len(observationIDs) == 0 {
		return nil, nil
	}
	var out []string
	for _, observationID := range observationIDs {
		spanIDs := spanIDsByObservation[observationID]
		obs, err := observations.Get(ctx, scope, observationID)
		if err != nil {
			return out, fmt.Errorf("graph ledger: get observation: %w", err)
		}
		spanIDs = appendObservationSpanIDs(spanIDs, obs)
		hasRemaining, err := linkedSpanHasRemainingLinksExceptAssertion(ctx, scope, spanIDs, links, node)
		if err != nil {
			return out, err
		}
		if hasRemaining {
			continue
		}
		obsNode := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: observationID}
		remaining, err := links.FindByNode(ctx, scope, obsNode)
		if err != nil {
			return out, fmt.Errorf("graph ledger: find observation links: %w", err)
		}
		if hasNonAssertionLink(remaining, node) {
			continue
		}
		out = append(out, observationID)
	}
	return out, nil
}

func LinkedObservationIDs(links []domain.FactLink, assertion domain.GraphNodeRef) []string {
	ids, _ := LinkedObservationAndSpanIDs(links, assertion)
	return ids
}

func LinkedObservationAndSpanIDs(links []domain.FactLink, assertion domain.GraphNodeRef) ([]string, map[string][]string) {
	seen := map[string]struct{}{}
	var out []string
	spanIDsByObservation := map[string][]string{}
	spanSeen := map[string]map[string]struct{}{}
	addObservation := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	addSpan := func(observationID, spanID string) {
		if observationID == "" || spanID == "" {
			return
		}
		addObservation(observationID)
		scopeSeen, ok := spanSeen[observationID]
		if !ok {
			scopeSeen = map[string]struct{}{}
			spanSeen[observationID] = scopeSeen
		}
		if _, ok := scopeSeen[spanID]; ok {
			return
		}
		scopeSeen[spanID] = struct{}{}
		spanIDsByObservation[observationID] = append(spanIDsByObservation[observationID], spanID)
	}
	for _, link := range links {
		if link.From == assertion {
			if link.To.Kind == domain.GraphNodeObservation {
				addObservation(link.To.ID)
			}
			if link.To.Kind == domain.GraphNodeObservationSpan {
				for _, ref := range link.EvidenceRefs {
					if ref.SpanID == link.To.ID {
						addSpan(ref.ObservationID, ref.SpanID)
					}
				}
			}
		}
		if link.To == assertion {
			if link.From.Kind == domain.GraphNodeObservation {
				addObservation(link.From.ID)
			}
			if link.From.Kind == domain.GraphNodeObservationSpan {
				for _, ref := range link.EvidenceRefs {
					if ref.SpanID == link.From.ID {
						addSpan(ref.ObservationID, ref.SpanID)
					}
				}
			}
		}
		for _, observationID := range link.EvidenceObservationIDs {
			addObservation(observationID)
		}
		for _, ref := range link.EvidenceRefs {
			addSpan(ref.ObservationID, ref.SpanID)
		}
	}
	return out, spanIDsByObservation
}

func appendObservationSpanIDs(spanIDs []string, obs domain.Observation) []string {
	seen := make(map[string]struct{}, len(spanIDs)+len(obs.Spans))
	out := make([]string, 0, len(spanIDs)+len(obs.Spans))
	for _, spanID := range spanIDs {
		if spanID == "" {
			continue
		}
		if _, ok := seen[spanID]; ok {
			continue
		}
		seen[spanID] = struct{}{}
		out = append(out, spanID)
	}
	for _, span := range obs.Spans {
		if span.ID == "" {
			continue
		}
		if _, ok := seen[span.ID]; ok {
			continue
		}
		seen[span.ID] = struct{}{}
		out = append(out, span.ID)
	}
	return out
}

func linkedSpanHasRemainingLinksExceptAssertion(ctx context.Context, scope domain.Scope, spanIDs []string, links port.LinkStore, assertion domain.GraphNodeRef) (bool, error) {
	for _, spanID := range spanIDs {
		remaining, err := links.FindByNode(ctx, scope, domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID})
		if err != nil {
			return false, fmt.Errorf("graph ledger: find observation span links: %w", err)
		}
		if hasNonAssertionLink(remaining, assertion) {
			return true, nil
		}
	}
	return false, nil
}

func hasNonAssertionLink(links []domain.FactLink, assertion domain.GraphNodeRef) bool {
	for _, link := range links {
		if link.From == assertion || link.To == assertion {
			continue
		}
		return true
	}
	return false
}
