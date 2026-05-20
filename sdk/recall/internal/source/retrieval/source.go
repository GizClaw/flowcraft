// Package retrieval implements the canonical retrieval CandidateSource.
//
// It searches the retrieval index that the retrieval projection
// writes to (see internal/projection/retrieval). The source layer
// strictly reads: it never mutates the index, and never short-circuits
// to TemporalFactStore — materialization is fusion's responsibility.
package retrieval

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Source is the retrieval candidate source. By default it runs BM25
// only; when an Embedder is supplied via WithEmbedder it embeds the
// query and emits a hybrid SearchRequest so the index can fuse cosine
// similarity with BM25.
type Source struct {
	index    retrieval.Index
	embedder embedding.Embedder
}

// Option configures the source at construction time. Options are
// additive — Source works without any (BM25-only).
type Option func(*Source)

// WithEmbedder enables hybrid search by embedding the query text. The
// embedder must be the same one used by the retrieval projection on
// the write path; mixing dimensions makes the cosine lane a no-op
// because the memory index requires len(QueryVector) == len(Doc.Vector).
func WithEmbedder(e embedding.Embedder) Option {
	return func(s *Source) {
		s.embedder = e
	}
}

// New constructs a Source. index ownership stays with the caller
// (the Memory facade); the source never closes it.
func New(index retrieval.Index, opts ...Option) *Source {
	s := &Source{index: index}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name implements port.Source.
func (s *Source) Name() string { return planner.SourceRetrieval }

// Query runs a text search restricted to the scope's namespace and
// applies the AgentID soft-isolation filter. When the caller's
// scope has a non-empty AgentID, results are limited to facts
// written by that agent OR shared facts (scope_agent_id == "");
// when AgentID is empty, no agent filter is applied (cross-agent
// recall).
func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
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
	if s.embedder != nil {
		if vec := s.embedQuery(ctx, plan.Intent.Text); len(vec) > 0 {
			req.QueryVector = vec
		}
	}

	started := time.Now()
	resp, err := s.index.Search(ctx, ns, req)
	latency := time.Since(started)
	if err != nil {
		return domain.SourceResult{
			Source:  s.Name(),
			Err:     err,
			Latency: latency,
		}
	}
	if resp == nil {
		return domain.SourceResult{Source: s.Name(), Latency: latency}
	}

	candidates := make([]domain.Candidate, 0, len(resp.Hits))
	for i, hit := range resp.Hits {
		factID := hit.Doc.ID
		if v, ok := hit.Doc.Metadata[domain.MetaFactID]; ok {
			if s, ok := v.(string); ok && s != "" {
				factID = s
			}
		}
		if factID == "" {
			continue
		}
		candidates = append(candidates, domain.Candidate{
			FactID: factID,
			Scope:  scope,
			Source: s.Name(),
			Rank:   i + 1,
			Score:  hit.Score,
			Metadata: map[string]any{
				"fact_kind": hit.Doc.Metadata[domain.MetaFactKind],
				"merge_key": hit.Doc.Metadata[domain.MetaMergeKey],
			},
		})
	}

	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  len(resp.Hits) >= budget,
		Latency:    latency,
	}
}

// embedQuery embeds the query text. Embedder errors degrade to BM25
// (the cosine lane simply contributes nothing); we never abort recall
// because the embedder is offline.
func (s *Source) embedQuery(ctx context.Context, text string) []float32 {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil
	}
	vec, err := s.embedder.Embed(ctx, t)
	if err != nil {
		return nil
	}
	return vec
}

// buildFilter assembles the scope-isolation filter. RuntimeID and
// UserID are hard partition keys (they participate in the namespace
// as well, but Eq filters here defend against any backend that
// shares the namespace); AgentID is soft.
func buildFilter(scope domain.Scope) retrieval.Filter {
	eq := map[string]any{
		domain.MetaScopeRT: scope.RuntimeID,
	}
	if scope.UserID != "" {
		eq[domain.MetaScopeUser] = scope.UserID
	}
	base := retrieval.Filter{Eq: eq}
	if scope.AgentID == "" {
		return base
	}
	// AgentID soft isolation: include facts written by this agent OR
	// shared facts (scope_agent_id == ""). Mirrors v1 AgentRecallFilter.
	agentFilter := retrieval.Filter{Or: []retrieval.Filter{
		{Eq: map[string]any{domain.MetaScopeAgent: scope.AgentID}},
		{Eq: map[string]any{domain.MetaScopeAgent: ""}},
	}}
	return retrieval.Filter{And: []retrieval.Filter{base, agentFilter}}
}
