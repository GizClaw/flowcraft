package graphledger

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// ClearAssertion removes all graph links touching an assertion and deletes
// orphaned observations that were only reachable through those links.
func ClearAssertion(ctx context.Context, scope domain.Scope, factID string, observations port.ObservationStore, links port.LinkStore) (int, int, error) {
	if links == nil || factID == "" {
		return 0, 0, nil
	}
	node := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	existing, err := links.FindByNode(ctx, scope, node)
	if err != nil {
		return 0, 0, fmt.Errorf("graph ledger: find assertion links: %w", err)
	}
	observationIDs, spanIDsByObservation := LinkedObservationAndSpanIDs(existing, node)
	linkCount, err := links.DeleteByNode(ctx, scope, node)
	if err != nil {
		return 0, 0, fmt.Errorf("graph ledger: delete assertion links: %w", err)
	}
	if observations == nil || len(observationIDs) == 0 {
		return linkCount, 0, nil
	}
	var deletedObservations int
	for _, observationID := range observationIDs {
		if linkedSpanHasRemainingLinks(ctx, scope, spanIDsByObservation[observationID], links) {
			continue
		}
		obsNode := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: observationID}
		remaining, err := links.FindByNode(ctx, scope, obsNode)
		if err != nil {
			return linkCount, deletedObservations, fmt.Errorf("graph ledger: find observation links: %w", err)
		}
		if len(remaining) > 0 {
			continue
		}
		if err := observations.Delete(ctx, scope, []string{observationID}); err != nil {
			return linkCount, deletedObservations, fmt.Errorf("graph ledger: delete observation: %w", err)
		}
		deletedObservations++
	}
	return linkCount, deletedObservations, nil
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

func linkedSpanHasRemainingLinks(ctx context.Context, scope domain.Scope, spanIDs []string, links port.LinkStore) bool {
	for _, spanID := range spanIDs {
		remaining, err := links.FindByNode(ctx, scope, domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID})
		if err == nil && len(remaining) > 0 {
			return true
		}
	}
	return false
}
