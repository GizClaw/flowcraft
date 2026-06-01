package observation

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

const (
	observationSourceCandidateCap = 6
	observationSourceSearchCap    = 24
	observationSourceMinScore     = 0.18
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
	queryTokens := plan.Intent.Features.Tokens
	if len(queryTokens) == 0 {
		queryTokens = recallintent.TextTokenSet(plan.Intent.Text)
	}
	scored := make([]observationSourceHit, 0, len(resp.Hits))
	seen := map[string]struct{}{}
	seenText := map[string]struct{}{}
	for _, hit := range resp.Hits {
		obsID, _ := hit.Doc.Metadata[MetaObservationID].(string)
		if obsID == "" {
			obsID = hit.Doc.ID
		}
		if obsID == "" {
			continue
		}
		textKey := observationSourceTextKey(hit.Doc.Content)
		scoreText := strings.TrimSpace(hit.Doc.Content)
		if speaker, _ := hit.Doc.Metadata["speaker"].(string); speaker != "" {
			scoreText = strings.TrimSpace(speaker + " " + scoreText)
		}
		textScore := observationSourceLexicalScore(queryTokens, plan.Intent.Text, scoreText)
		if textScore < observationSourceMinScore {
			continue
		}
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
		scored = append(scored, observationSourceHit{score: textScore, candidate: domain.Candidate{
			Kind:        domain.GraphNodeObservation,
			ID:          obsID,
			Scope:       plan.Intent.Scope,
			Source:      s.Name(),
			Score:       textScore,
			EvidenceIDs: evidenceIDs,
			Metadata: map[string]any{
				"sources": []string{s.Name()},
			},
		}})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].candidate.ID < scored[j].candidate.ID
	})
	if len(scored) > candidateCap {
		scored = scored[:candidateCap]
	}
	candidates := make([]domain.Candidate, 0, len(scored))
	for i, hit := range scored {
		candidate := hit.candidate
		candidate.Rank = i + 1
		candidates = append(candidates, candidate)
	}
	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  len(resp.Hits) >= searchTopK,
		Latency:    latency,
	}
}

type observationSourceHit struct {
	score     float64
	candidate domain.Candidate
}

func observationSourceTextKey(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

func observationSourceLexicalScore(queryTokens map[string]struct{}, query, text string) float64 {
	text = strings.TrimSpace(text)
	if text == "" || len(queryTokens) == 0 {
		return 0
	}
	textTokens := recallintent.TextTokenSet(text)
	overlap := 0
	for token := range queryTokens {
		if _, ok := textTokens[token]; ok {
			overlap++
		}
	}
	queryNorm := observationSourceTextKey(query)
	textNorm := observationSourceTextKey(text)
	phraseBonus := 0.0
	if queryNorm != "" && strings.Contains(textNorm, queryNorm) {
		phraseBonus = 0.35
	}
	if overlap < 2 && phraseBonus == 0 {
		return 0
	}
	return min(1, float64(overlap)/float64(len(queryTokens))+phraseBonus)
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
