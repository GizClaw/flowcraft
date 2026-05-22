// Package entity implements the entity-mention CandidateSource.
//
// It reads from the entity projection's inverted index (see
// internal/projection/entity). Reads only — the source never
// mutates projection state (docs §16 invariant).
package entity

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lookup is the minimal read contract this source needs from the
// entity projection. Keeping it narrow lets the source depend on a
// pure interface rather than the projection package, and gives
// tests an easy mock.
type Lookup interface {
	Lookup(ctx context.Context, scope domain.Scope, entities []string) []string
}

// Source surfaces fact ids that mention any of the requested
// entities, ordered as the lookup returns them.
type Source struct {
	lookup Lookup
	// BaseScore is the score assigned to every entity hit. Entity
	// matches are binary by construction; PR-3 leaves ranking
	// inside fusion via the RRF weight.
	BaseScore float64
}

// NewSource constructs a Source backed by lookup.
func NewSource(lookup Lookup) *Source {
	return &Source{lookup: lookup, BaseScore: 1.0}
}

// Name implements port.Source.
func (s *Source) Name() string { return planner.SourceEntity }

// Query returns at most plan.SourceBudgets[entity] candidates. When
// the plan carries no entity hints (Intent.Entities empty), Query
// short-circuits to an empty result so the source is safe to wire
// unconditionally even though the planner only includes it
// conditionally.
func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	ents := plan.Intent.Entities
	if len(ents) == 0 {
		return domain.SourceResult{Source: s.Name()}
	}

	scope := plan.Intent.Scope
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		budget = plan.TotalCap
	}
	if budget <= 0 {
		budget = planner.DefaultLimit
	}

	started := time.Now()
	ids := s.lookup.Lookup(ctx, scope, ents)
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
