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

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/embedding"
)

// Source is the retrieval candidate source. By default it runs BM25
// only; when an Embedder is supplied via WithEmbedder it embeds the
// query and emits a hybrid SearchRequest so the index can fuse cosine
// similarity with BM25.
type Source struct {
	index    retrieval.Index
	embedder embedding.Embedder
}

// SourceOption configures the source at construction time.
type SourceOption func(*Source)

// WithSourceEmbedder enables hybrid search by embedding the query text.
func WithSourceEmbedder(e embedding.Embedder) SourceOption {
	return func(s *Source) {
		s.embedder = e
	}
}

// NewSource constructs a Source. index ownership stays with the caller.
func NewSource(index retrieval.Index, opts ...SourceOption) *Source {
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
	queryText := canonicalRetrievalQueryText(plan.Intent)
	if strings.TrimSpace(queryText) == "" {
		return domain.SourceResult{Source: s.Name()}
	}
	scope := plan.Intent.Scope
	ns := NamespaceFor(scope)
	budget := plan.SourceBudgets[s.Name()]
	if budget <= 0 {
		budget = plan.TotalCap
	}
	if budget <= 0 {
		budget = planner.DefaultLimit
	}

	req := retrieval.SearchRequest{
		QueryText: queryText,
		TopK:      budget,
		Filter:    buildFilter(scope),
	}
	if s.embedder != nil {
		if vec := s.embedQuery(ctx, queryText); len(vec) > 0 {
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
	byFactID := make(map[string]int, len(resp.Hits))
	for _, hit := range resp.Hits {
		factID := hit.Doc.ID
		if v, ok := hit.Doc.Metadata[domain.MetaFactID]; ok {
			if s, ok := v.(string); ok && s != "" {
				factID = s
			}
		}
		if factID == "" {
			continue
		}
		evidenceIDs := retrievalCandidateEvidenceIDs(hit.Doc.Metadata)
		if existing, ok := byFactID[factID]; ok {
			candidates[existing].EvidenceIDs = mergeRetrievalEvidenceIDs(candidates[existing].EvidenceIDs, evidenceIDs)
			if hit.Score > candidates[existing].Score {
				candidates[existing].Score = hit.Score
				candidates[existing].DiscoverySignals = retrievalDiscoverySignals(s.Name(), retrievalDiscoveryKind(req), hit.Score)
			}
			continue
		}
		meta := retrievalCandidateMeta(hit.Doc.Metadata)
		candidates = append(candidates, domain.Candidate{
			Kind:             domain.GraphNodeAssertion,
			ID:               factID,
			Scope:            scope,
			Source:           s.Name(),
			Rank:             len(candidates) + 1,
			Score:            hit.Score,
			EvidenceIDs:      evidenceIDs,
			DiscoverySignals: retrievalDiscoverySignals(s.Name(), retrievalDiscoveryKind(req), hit.Score),
			Metadata:         meta,
		})
		byFactID[factID] = len(candidates) - 1
	}

	return domain.SourceResult{
		Source:     s.Name(),
		Candidates: candidates,
		Truncated:  len(resp.Hits) >= budget,
		Latency:    latency,
	}
}

func canonicalRetrievalQueryText(intent domain.QueryIntent) string {
	if text := strings.TrimSpace(intent.Text); text != "" {
		return text
	}
	if strings.TrimSpace(intent.Subject) == "" &&
		strings.TrimSpace(intent.Predicate) == "" &&
		strings.TrimSpace(intent.Object) == "" &&
		len(intent.Entities) == 0 &&
		len(intent.Kinds) == 0 &&
		intent.TimeRange.IsZero() {
		return ""
	}
	parts := make([]string, 0, 6+len(intent.Entities)+len(intent.Kinds))
	parts = appendNonEmpty(parts, intent.Subject, intent.Predicate, intent.Object)
	parts = append(parts, intent.Entities...)
	for _, kind := range intent.Kinds {
		if kind != "" {
			parts = append(parts, string(kind))
		}
	}
	if !intent.TimeRange.IsZero() {
		if !intent.TimeRange.From.IsZero() {
			parts = append(parts, intent.TimeRange.From.Format("2006-01-02"))
		}
		if !intent.TimeRange.To.IsZero() {
			parts = append(parts, intent.TimeRange.To.Format("2006-01-02"))
		}
	}
	return strings.Join(parts, " ")
}

func appendNonEmpty(out []string, values ...string) []string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// embedQuery embeds the query text. Embedder errors degrade to BM25
// (the cosine lane simply contributes nothing); we never abort recall
// because the embedder is offline.
func retrievalCandidateMeta(docMeta map[string]any) map[string]any {
	meta := map[string]any{
		"fact_kind":        docMeta[domain.MetaFactKind],
		"merge_key":        docMeta[domain.MetaMergeKey],
		DocKindMetadataKey: docMeta[DocKindMetadataKey],
	}
	if v, ok := docMeta[EvidenceIDMetadataKey]; ok {
		meta[EvidenceIDMetadataKey] = v
	}
	if v, ok := docMeta[domain.MetaReinforcement]; ok {
		meta[domain.MetaReinforcement] = v
	}
	if v, ok := docMeta[domain.MetaPenalty]; ok {
		meta[domain.MetaPenalty] = v
	}
	return meta
}

func retrievalCandidateEvidenceIDs(docMeta map[string]any) []string {
	if raw, ok := docMeta[EvidenceIDMetadataKey]; ok {
		if id, ok := raw.(string); ok && strings.TrimSpace(id) != "" {
			return []string{strings.TrimSpace(id)}
		}
	}
	return nil
}

func retrievalDiscoveryKind(req retrieval.SearchRequest) string {
	if len(req.QueryVector) > 0 {
		return "vector"
	}
	return "bm25"
}

func retrievalDiscoverySignals(source, kind string, score float64) []domain.DiscoverySignal {
	return []domain.DiscoverySignal{{
		Source: source,
		Kind:   kind,
		Value:  "retrieval_score",
		Score:  score,
	}}
}

func mergeRetrievalEvidenceIDs(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(out)+len(incoming))
	for _, id := range out {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, id := range incoming {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

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
