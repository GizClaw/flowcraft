package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

type overBudgetTraverse struct {
	ids       []string
	lastLimit int
}

func (s *overBudgetTraverse) Traverse(_ context.Context, _ model.Scope, _ []string, _, limit int) []string {
	s.lastLimit = limit
	ids := append([]string(nil), s.ids...)
	if limit > 0 && len(ids) > limit {
		return ids[:limit]
	}
	return ids
}

func TestSource_CapsAndMarksTruncated(t *testing.T) {
	traverse := &overBudgetTraverse{ids: []string{"a", "b", "c"}}
	src := New(traverse)
	plan := model.QueryPlan{
		Intent: model.QueryIntent{
			Scope:        model.Scope{RuntimeID: "rt", UserID: "u1"},
			Entities:     []string{"alice"},
			GraphEnabled: true,
			Limit:        2,
		},
		SourceBudgets: map[string]int{planner.SourceGraph: 2},
		TotalCap:      2,
	}

	got := src.Query(context.Background(), plan)
	if len(got.Candidates) != 2 {
		t.Fatalf("graph source must cap candidates to its budget, got %+v", got.Candidates)
	}
	if !got.Truncated {
		t.Fatalf("graph source must mark truncated when traversal returns more than budget")
	}
	if traverse.lastLimit != 3 {
		t.Fatalf("graph source should request budget+1 from traversal, got limit %d", traverse.lastLimit)
	}
}
