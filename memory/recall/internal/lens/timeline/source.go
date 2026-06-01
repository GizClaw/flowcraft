// Package timeline implements the timeline CandidateSource.
package timeline

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Querier is the read contract from the timeline projection.
type Querier interface {
	Query(ctx context.Context, scope domain.Scope, from, to time.Time, kinds []domain.FactKind, limit int) []string
}

// Source surfaces fact ids from the timeline projection ordered by
// effective time.
type Source struct {
	querier   Querier
	BaseScore float64
}

// NewSource constructs a Source.
func NewSource(querier Querier) *Source {
	return &Source{querier: querier, BaseScore: 1.0}
}

func (s *Source) Name() string { return planner.SourceTimeline }

func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	if !planner.ActivatesTimeline(plan.Intent) {
		return domain.SourceResult{Source: s.Name()}
	}
	scope := plan.Intent.Scope
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		return domain.SourceResult{Source: s.Name()}
	}

	tr := plan.Intent.TimeRange
	queryLimit := budget + 1
	if agentSoftIsolationQuery(scope) {
		queryLimit = 0
	}
	started := time.Now()
	ids := s.querier.Query(ctx, scope, tr.From, tr.To, plan.Intent.Kinds, queryLimit)
	latency := time.Since(started)

	truncated := false
	if !agentSoftIsolationQuery(scope) && len(ids) > budget {
		ids = ids[:budget]
		truncated = true
	}

	candidates := make([]domain.Candidate, 0, len(ids))
	for i, id := range ids {
		candidates = append(candidates, domain.Candidate{
			Kind:   domain.GraphNodeAssertion,
			ID:     id,
			Scope:  scope,
			Source: s.Name(),
			Rank:   i + 1,
			Score:  s.BaseScore,
		})
	}
	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  truncated,
		Latency:    latency,
	}
}

func agentSoftIsolationQuery(scope domain.Scope) bool {
	return scope.AgentID != ""
}
