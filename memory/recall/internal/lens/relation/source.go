// Package relation implements the relation CandidateSource.
package relation

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lookup is the read contract from the relation projection.
type Lookup interface {
	Lookup(ctx context.Context, scope domain.Scope, subject, predicate, object string) []string
}

type boundedLookup interface {
	LookupLimit(ctx context.Context, scope domain.Scope, subject, predicate, object string, limit int) []string
}

// Source surfaces fact ids matching typed relation dimensions.
type Source struct {
	lookup    Lookup
	BaseScore float64
}

// New constructs a Source.
func NewSource(lookup Lookup) *Source {
	return &Source{lookup: lookup, BaseScore: 1.0}
}

func (s *Source) Name() string { return planner.SourceRelation }

func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	intent := plan.Intent
	if !planner.ActivatesRelation(intent) {
		return domain.SourceResult{Source: s.Name()}
	}
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		return domain.SourceResult{Source: s.Name()}
	}

	started := time.Now()
	var ids []string
	if bounded, ok := s.lookup.(boundedLookup); ok {
		ids = bounded.LookupLimit(ctx, intent.Scope, intent.Subject, intent.Predicate, intent.Object, budget+1)
	} else {
		ids = s.lookup.Lookup(ctx, intent.Scope, intent.Subject, intent.Predicate, intent.Object)
	}
	latency := time.Since(started)

	truncated := false
	if len(ids) > budget {
		ids = ids[:budget]
		truncated = true
	}

	candidates := make([]domain.Candidate, 0, len(ids))
	for i, id := range ids {
		candidates = append(candidates, domain.Candidate{
			Kind:   domain.GraphNodeAssertion,
			ID:     id,
			Scope:  intent.Scope,
			Source: s.Name(),
			Rank:   i + 1,
			Score:  s.BaseScore,
			DiscoverySignals: []domain.DiscoverySignal{{
				Source: s.Name(),
				Kind:   "relation_exact",
				Value:  "typed_triple",
				Score:  s.BaseScore,
			}},
		})
	}
	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  truncated,
		Latency:    latency,
	}
}
