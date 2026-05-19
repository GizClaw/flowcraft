// Package profile implements the profile CandidateSource.
package profile

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

// Lookup is the read contract from the profile projection.
type Lookup interface {
	Lookup(ctx context.Context, scope model.Scope, subject string) []string
}

// Source surfaces active-slot facts for a subject.
type Source struct {
	lookup    Lookup
	BaseScore float64
}

// New constructs a Source.
func New(lookup Lookup) *Source {
	return &Source{lookup: lookup, BaseScore: 1.0}
}

func (s *Source) Name() string { return planner.SourceProfile }

func (s *Source) Query(ctx context.Context, plan model.QueryPlan) model.SourceResult {
	if !planner.ActivatesProfile(plan.Intent) {
		return model.SourceResult{Source: s.Name()}
	}
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		return model.SourceResult{Source: s.Name()}
	}

	started := time.Now()
	ids := s.lookup.Lookup(ctx, plan.Intent.Scope, plan.Intent.Subject)
	latency := time.Since(started)

	truncated := false
	if len(ids) > budget {
		ids = ids[:budget]
		truncated = true
	}

	candidates := make([]model.Candidate, 0, len(ids))
	for i, id := range ids {
		candidates = append(candidates, model.Candidate{
			FactID: id,
			Scope:  plan.Intent.Scope,
			Source: s.Name(),
			Rank:   i + 1,
			Score:  s.BaseScore,
		})
	}
	return model.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  truncated,
		Latency:    latency,
	}
}
