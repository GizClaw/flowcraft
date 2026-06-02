package observation

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

const (
	observationSourceCandidateCap = 6
	observationSourceSearchCap    = 24
)

type Source struct {
	index retrieval.Index
}

func NewSource(index retrieval.Index) *Source {
	return &Source{index: index}
}

func (s *Source) Name() string { return planner.SourceObservation }

func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	if s == nil || s.index == nil {
		return domain.SourceResult{Source: planner.SourceObservation}
	}
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		budget = max(1, plan.TotalCap/2)
	}
	if budget <= 0 {
		budget = planner.DefaultLimit
	}
	candidateCap := min(observationSourceCandidateCap, max(2, budget/3))
	searchTopK := min(observationSourceSearchCap, max(candidateCap*2, candidateCap))
	req := retrieval.SearchRequest{
		QueryText: plan.Intent.Text,
		TopK:      searchTopK,
		Filter:    buildFilter(plan.Intent.Scope),
	}
	started := time.Now()
	resp, err := s.index.Search(ctx, NamespaceFor(plan.Intent.Scope), req)
	latency := time.Since(started)
	if err != nil {
		return domain.SourceResult{Source: s.Name(), Err: err, Latency: latency}
	}
	if resp == nil {
		return domain.SourceResult{Source: s.Name(), Latency: latency}
	}
	candidates := make([]domain.Candidate, 0, candidateCap)
	seen := map[string]struct{}{}
	seenText := map[string]struct{}{}
	for _, hit := range resp.Hits {
		if len(candidates) >= candidateCap {
			break
		}
		obsID, _ := hit.Doc.Metadata[MetaObservationID].(string)
		if obsID == "" {
			obsID = hit.Doc.ID
		}
		if obsID == "" {
			continue
		}
		textKey := observationSourceTextKey(hit.Doc.Content)
		if textKey != "" {
			if _, ok := seenText[textKey]; ok {
				continue
			}
		}
		if _, ok := seen[obsID]; ok {
			continue
		}
		seen[obsID] = struct{}{}
		if textKey != "" {
			seenText[textKey] = struct{}{}
		}
		evidenceIDs := []string{obsID}
		if spanID, _ := hit.Doc.Metadata[MetaSpanID].(string); spanID != "" {
			evidenceIDs = append(evidenceIDs, spanID)
		}
		candidates = append(candidates, domain.Candidate{
			Kind:        domain.GraphNodeObservation,
			ID:          obsID,
			Scope:       plan.Intent.Scope,
			Source:      s.Name(),
			Rank:        len(candidates) + 1,
			Score:       0,
			EvidenceIDs: evidenceIDs,
			Metadata: map[string]any{
				"sources": []string{s.Name()},
			},
		})
	}
	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  len(resp.Hits) >= searchTopK,
		Latency:    latency,
	}
}

func observationSourceTextKey(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

func buildFilter(scope domain.Scope) retrieval.Filter {
	f := retrieval.Filter{
		Eq: map[string]any{
			MetaScopeRT:   scope.RuntimeID,
			MetaScopeUser: scope.UserID,
		},
	}
	if scope.AgentID != "" {
		f.Or = []retrieval.Filter{
			{Eq: map[string]any{MetaScopeAgent: scope.AgentID}},
			{Eq: map[string]any{MetaScopeAgent: ""}},
		}
	}
	return f
}
