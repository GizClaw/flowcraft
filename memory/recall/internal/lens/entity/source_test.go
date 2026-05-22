package entity

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

type stubLookup struct {
	want []string
}

func (s stubLookup) Lookup(_ context.Context, _ domain.Scope, _ []string) []string {
	return s.want
}

func TestSource_NoEntitiesShortCircuits(t *testing.T) {
	src := NewSource(stubLookup{want: []string{"a", "b"}})
	res := src.Query(context.Background(), domain.QueryPlan{
		Intent: domain.QueryIntent{Scope: domain.Scope{RuntimeID: "rt"}},
	})
	if len(res.Candidates) != 0 {
		t.Errorf("entity source must noop without entity hints, got %+v", res.Candidates)
	}
}

func TestSource_BudgetCapsCandidates(t *testing.T) {
	src := NewSource(stubLookup{want: []string{"a", "b", "c", "d"}})
	res := src.Query(context.Background(), domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:    domain.Scope{RuntimeID: "rt"},
			Entities: []string{"alice"},
		},
		SourceBudgets: map[string]int{planner.SourceEntity: 2},
	})
	if len(res.Candidates) != 2 {
		t.Fatalf("expected budget to clamp to 2, got %d", len(res.Candidates))
	}
	if !res.Truncated {
		t.Error("expected Truncated to be true when budget was hit")
	}
	if res.Candidates[0].FactID != "a" || res.Candidates[0].Rank != 1 {
		t.Errorf("rank/order wrong: %+v", res.Candidates)
	}
}
