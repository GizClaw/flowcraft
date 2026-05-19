package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestSource_AgentIDSoftIsolationFilter(t *testing.T) {
	idx := retrievalmem.New()
	proj, _ := retrievalproj.New(idx)
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}

	mk := func(id, agent, body string) model.TemporalFact {
		s := scope
		s.AgentID = agent
		return model.TemporalFact{
			ID:      id,
			Scope:   s,
			Kind:    model.KindNote,
			Content: body,
		}
	}
	if err := proj.Project(context.Background(), []model.TemporalFact{
		mk("a", "agent-a", "alpha secret"),
		mk("b", "agent-b", "alpha secret"),
		mk("s", "", "alpha secret"),
	}); err != nil {
		t.Fatal(err)
	}

	source := New(idx)

	// agent-a query: must include own + shared, exclude agent-b.
	plan := model.QueryPlan{
		Intent:        model.QueryIntent{Text: "alpha", Scope: model.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}, Limit: 10},
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
	plan.Intent.Scope = model.Scope{RuntimeID: "rt", UserID: "u1"}
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
	proj, _ := retrievalproj.New(idx)
	scope := model.Scope{RuntimeID: "rt"}
	if err := proj.Project(context.Background(), []model.TemporalFact{
		{ID: "f1", Scope: scope, Kind: model.KindNote, Content: "alpha beta"},
	}); err != nil {
		t.Fatal(err)
	}
	s := New(idx)
	res := s.Query(context.Background(), model.QueryPlan{
		Intent:        model.QueryIntent{Text: "alpha", Scope: scope, Limit: 5},
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
var _ retrieval.Index = (*retrievalmem.Index)(nil)
