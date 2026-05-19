// Package retrieval implements the canonical retrieval CandidateSource.
//
// It searches the retrieval index that the retrieval projection
// writes to (see internal/projection/retrieval). The source layer
// strictly reads: it never mutates the index, and never short-circuits
// to TemporalFactStore — materialization is fusion's responsibility.
package retrieval

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Source is the BM25-only retrieval candidate source. PR-3 does not
// wire embeddings; hybrid mode lands in later phases.
type Source struct {
	index retrieval.Index
}

// New constructs a Source. index ownership stays with the caller
// (the Memory facade); the source never closes it.
func New(index retrieval.Index) *Source {
	return &Source{index: index}
}

// Name implements source.CandidateSource.
func (s *Source) Name() string { return planner.SourceRetrieval }

// Query runs a text search restricted to the scope's namespace and
// applies the AgentID soft-isolation filter. When the caller's
// scope has a non-empty AgentID, results are limited to facts
// written by that agent OR shared facts (scope_agent_id == "");
// when AgentID is empty, no agent filter is applied (cross-agent
// recall).
func (s *Source) Query(ctx context.Context, plan model.QueryPlan) model.SourceResult {
	scope := plan.Intent.Scope
	ns := retrievalproj.NamespaceFor(scope)
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		budget = plan.TotalCap
	}
	if budget <= 0 {
		budget = planner.DefaultLimit
	}

	req := retrieval.SearchRequest{
		QueryText: plan.Intent.Text,
		TopK:      budget,
		Filter:    buildFilter(scope),
	}

	started := time.Now()
	resp, err := s.index.Search(ctx, ns, req)
	latency := time.Since(started)
	if err != nil {
		return model.SourceResult{
			Source:  s.Name(),
			Err:     err,
			Latency: latency,
		}
	}
	if resp == nil {
		return model.SourceResult{Source: s.Name(), Latency: latency}
	}

	candidates := make([]model.Candidate, 0, len(resp.Hits))
	for i, hit := range resp.Hits {
		factID := hit.Doc.ID
		if v, ok := hit.Doc.Metadata[model.MetaFactID]; ok {
			if s, ok := v.(string); ok && s != "" {
				factID = s
			}
		}
		if factID == "" {
			continue
		}
		candidates = append(candidates, model.Candidate{
			FactID: factID,
			Scope:  scope,
			Source: s.Name(),
			Rank:   i + 1,
			Score:  hit.Score,
			Metadata: map[string]any{
				"fact_kind": hit.Doc.Metadata[model.MetaFactKind],
				"merge_key": hit.Doc.Metadata[model.MetaMergeKey],
			},
		})
	}

	return model.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  len(resp.Hits) >= budget,
		Latency:    latency,
	}
}

// buildFilter assembles the scope-isolation filter. RuntimeID and
// UserID are hard partition keys (they participate in the namespace
// as well, but Eq filters here defend against any backend that
// shares the namespace); AgentID is soft.
func buildFilter(scope model.Scope) retrieval.Filter {
	eq := map[string]any{
		model.MetaScopeRT: scope.RuntimeID,
	}
	if scope.UserID != "" {
		eq[model.MetaScopeUser] = scope.UserID
	}
	base := retrieval.Filter{Eq: eq}
	if scope.AgentID == "" {
		return base
	}
	// AgentID soft isolation: include facts written by this agent OR
	// shared facts (scope_agent_id == ""). Mirrors v1 AgentRecallFilter.
	agentFilter := retrieval.Filter{Or: []retrieval.Filter{
		{Eq: map[string]any{model.MetaScopeAgent: scope.AgentID}},
		{Eq: map[string]any{model.MetaScopeAgent: ""}},
	}}
	return retrieval.Filter{And: []retrieval.Filter{base, agentFilter}}
}
