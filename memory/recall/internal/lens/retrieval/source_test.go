package retrieval_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	retlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	sdkretrieval "github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

type sourceStubEmbedder struct {
	dim   int
	calls int
}

func (s *sourceStubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	s.calls++
	vec := make([]float32, s.dim)
	for i := 0; i < len(text) && i < s.dim; i++ {
		vec[i] = float32(text[i])
	}
	return vec, nil
}

func (s *sourceStubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := s.Embed(nil, t)
		out[i] = v
	}
	return out, nil
}

func TestSource_WithEmbedder_InvokesEmbedOnQuery(t *testing.T) {
	idx := retrievalmem.New()
	emb := &sourceStubEmbedder{dim: 8}
	proj, _ := retlens.New(idx, retlens.WithEmbedder(emb))
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	if err := proj.Project(context.Background(), []domain.TemporalFact{
		{ID: "a", Scope: scope, Kind: domain.KindNote, Content: "hello world"},
	}); err != nil {
		t.Fatal(err)
	}

	src := retlens.NewSource(idx, retlens.WithSourceEmbedder(emb))
	plan := domain.QueryPlan{
		Intent:        domain.QueryIntent{Text: "hello", Scope: scope, Limit: 10},
		SourceOrder:   []string{planner.SourceRetrieval},
		SourceBudgets: map[string]int{planner.SourceRetrieval: 10},
		TotalCap:      10,
	}
	callsBefore := emb.calls
	res := src.Query(context.Background(), plan)
	if res.Err != nil {
		t.Fatalf("query: %v", res.Err)
	}
	if emb.calls <= callsBefore {
		t.Fatalf("expected embedder.Embed to be called on query, calls=%d", emb.calls)
	}
}

func TestSource_AgentIDSoftIsolationFilter(t *testing.T) {
	idx := retrievalmem.New()
	proj, _ := retlens.New(idx)
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}

	mk := func(id, agent, body string) domain.TemporalFact {
		s := scope
		s.AgentID = agent
		return domain.TemporalFact{
			ID:      id,
			Scope:   s,
			Kind:    domain.KindNote,
			Content: body,
		}
	}
	if err := proj.Project(context.Background(), []domain.TemporalFact{
		mk("a", "agent-a", "alpha secret"),
		mk("b", "agent-b", "alpha secret"),
		mk("s", "", "alpha secret"),
	}); err != nil {
		t.Fatal(err)
	}

	source := retlens.NewSource(idx)

	// agent-a query: must include own + shared, exclude agent-b.
	plan := domain.QueryPlan{
		Intent:        domain.QueryIntent{Text: "alpha", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}, Limit: 10},
		SourceOrder:   []string{planner.SourceRetrieval},
		SourceBudgets: map[string]int{planner.SourceRetrieval: 10},
		TotalCap:      10,
	}
	res := source.Query(context.Background(), plan)
	if res.Err != nil {
		t.Fatalf("source error: %v", res.Err)
	}
	seen := map[string]bool{}
	for _, c := range res.Candidates {
		seen[c.FactID] = true
	}
	if !seen["a"] || !seen["s"] {
		t.Errorf("agent-a query missing own/shared: %+v", seen)
	}
	if seen["b"] {
		t.Errorf("agent-a query leaked agent-b: %+v", seen)
	}

	// cross-agent query: AgentID empty -> all three visible.
	plan.Intent.Scope = domain.Scope{RuntimeID: "rt", UserID: "u1"}
	res = source.Query(context.Background(), plan)
	seen = map[string]bool{}
	for _, c := range res.Candidates {
		seen[c.FactID] = true
	}
	for _, want := range []string{"a", "b", "s"} {
		if !seen[want] {
			t.Errorf("cross-agent missing %q in %+v", want, seen)
		}
	}
}

func TestSource_PropagatesRetrievalScore(t *testing.T) {
	idx := retrievalmem.New()
	proj, _ := retlens.New(idx)
	scope := domain.Scope{RuntimeID: "rt"}
	if err := proj.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindNote, Content: "alpha beta"},
	}); err != nil {
		t.Fatal(err)
	}
	s := retlens.NewSource(idx)
	res := s.Query(context.Background(), domain.QueryPlan{
		Intent:        domain.QueryIntent{Text: "alpha", Scope: scope, Limit: 5},
		SourceBudgets: map[string]int{planner.SourceRetrieval: 5},
		TotalCap:      5,
	})
	if len(res.Candidates) == 0 {
		t.Fatalf("expected at least one candidate, got %+v", res)
	}
	if res.Candidates[0].FactID != "f1" {
		t.Errorf("fact id = %q", res.Candidates[0].FactID)
	}
}

// compile-time guard for the source contract shape.
var _ sdkretrieval.Index = retrievalmem.New()
