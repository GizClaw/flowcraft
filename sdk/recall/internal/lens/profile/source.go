// Package profile implements the profile CandidateSource.
package profile

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

// Lookup is the read contract from the profile projection.
type Lookup interface {
	Lookup(ctx context.Context, scope domain.Scope, subject string) []string
}

// Source surfaces active-slot facts for a subject.
type Source struct {
	lookup    Lookup
	BaseScore float64
}

// New constructs a Source.
func NewSource(lookup Lookup) *Source {
	return &Source{lookup: lookup, BaseScore: 1.0}
}

func (s *Source) Name() string { return planner.SourceProfile }

func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	if !planner.ActivatesProfile(plan.Intent) {
		return domain.SourceResult{Source: s.Name()}
	}
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		return domain.SourceResult{Source: s.Name()}
	}

	started := time.Now()
	ids := s.lookup.Lookup(ctx, plan.Intent.Scope, plan.Intent.Subject)
	latency := time.Since(started)

	truncated := false
	if len(ids) > budget {
		ids = ids[:budget]
		truncated = true
	}

	candidates := make([]domain.Candidate, 0, len(ids))
	for i, id := range ids {
		candidates = append(candidates, domain.Candidate{
			FactID: id,
			Scope:  plan.Intent.Scope,
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
