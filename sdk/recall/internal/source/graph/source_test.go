package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

type overBudgetTraverse struct {
	ids []string
}

func (s overBudgetTraverse) Traverse(context.Context, model.Scope, []string, int, int) []string {
	return append([]string(nil), s.ids...)
}

func TestSource_CapsAndMarksTruncated(t *testing.T) {
	src := New(overBudgetTraverse{ids: []string{"a", "b", "c"}})
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
}
