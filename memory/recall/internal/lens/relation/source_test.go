package relation

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

type stubRelationLookup struct {
	want []string
}

func (s stubRelationLookup) Lookup(_ context.Context, _ domain.Scope, _, _, _ string) []string {
	return s.want
}

func TestSource_BudgetCapsCandidates(t *testing.T) {
	src := NewSource(stubRelationLookup{want: []string{"a", "b", "c"}})
	res := src.Query(context.Background(), domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Subject:   "alice",
			Predicate: "knows",
		},
		SourceBudgets: map[string]int{planner.SourceRelation: 1},
	})

	if len(res.Candidates) != 1 || res.Candidates[0].ID != "a" {
		t.Fatalf("expected relation source to clamp to budget, got %+v", res.Candidates)
	}
	if !res.Truncated {
		t.Fatal("expected truncated relation result")
	}
}

func TestSource_AgentScopedQueryDefersBudgetUntilMaterialize(t *testing.T) {
	src := NewSource(stubRelationLookup{want: []string{"private-a", "private-b", "visible"}})
	res := src.Query(context.Background(), domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"},
			Subject:   "alice",
			Predicate: "knows",
		},
		SourceBudgets: map[string]int{planner.SourceRelation: 1},
	})

	if len(res.Candidates) != 3 {
		t.Fatalf("agent-scoped relation source must defer cap until materialize, got %+v", res.Candidates)
	}
	if res.Truncated {
		t.Fatal("agent-scoped relation source should not be truncated before materialize")
	}
}
