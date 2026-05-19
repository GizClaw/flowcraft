// Package graph implements the graph CandidateSource (docs §8.4).
package graph

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	graphproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/graph"
)

// Traverse is the read contract from the graph projection.
type Traverse interface {
	Traverse(ctx context.Context, scope model.Scope, seeds []string, maxHops, limit int) []string
}

// Source surfaces fact ids reachable via bounded graph expansion.
type Source struct {
	traverse  Traverse
	BaseScore float64
}

// New constructs a Source backed by traverse.
func New(traverse Traverse) *Source {
	return &Source{traverse: traverse, BaseScore: 0.85}
}

func (s *Source) Name() string { return planner.SourceGraph }

func (s *Source) Query(ctx context.Context, plan model.QueryPlan) model.SourceResult {
	if !planner.ActivatesGraph(plan.Intent) {
		return model.SourceResult{Source: s.Name()}
	}
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		return model.SourceResult{Source: s.Name()}
	}

	hops := graphproj.CapGraphHops(plan.Intent.GraphHops)

	started := time.Now()
	ids := s.traverse.Traverse(ctx, plan.Intent.Scope, plan.Intent.Entities, hops, budget+1)
	latency := time.Since(started)

	truncated := false
	if budget > 0 && len(ids) > budget {
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
