package timeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

type fixedQuerier struct {
	ids       []string
	lastLimit int
}

func (q *fixedQuerier) Query(_ context.Context, _ domain.Scope, _, _ time.Time, _ []domain.FactKind, limit int) []string {
	q.lastLimit = limit
	ids := append([]string(nil), q.ids...)
	if limit > 0 && len(ids) > limit {
		return ids[:limit]
	}
	return ids
}

func TestSource_DoesNotMarkExactBudgetAsTruncated(t *testing.T) {
	q := &fixedQuerier{ids: []string{"a", "b"}}
	src := NewSource(q)
	plan := domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Kinds:     []domain.FactKind{domain.KindEvent},
			TimeRange: domain.TimeRange{From: time.Unix(1, 0), To: time.Unix(2, 0)},
		},
		SourceBudgets: map[string]int{planner.SourceTimeline: 2},
		TotalCap:      2,
	}

	got := src.Query(context.Background(), plan)
	if got.Truncated {
		t.Fatalf("exact-budget timeline result should not be truncated: %+v", got)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
	if q.lastLimit != 3 {
		t.Fatalf("timeline source should request budget+1, got limit %d", q.lastLimit)
	}
}

func TestSource_MarksOverBudgetAsTruncated(t *testing.T) {
	q := &fixedQuerier{ids: []string{"a", "b", "c"}}
	src := NewSource(q)
	plan := domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Kinds:     []domain.FactKind{domain.KindEvent},
			TimeRange: domain.TimeRange{From: time.Unix(1, 0), To: time.Unix(2, 0)},
		},
		SourceBudgets: map[string]int{planner.SourceTimeline: 2},
		TotalCap:      2,
	}

	got := src.Query(context.Background(), plan)
	if !got.Truncated {
		t.Fatalf("over-budget timeline result should be truncated")
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("timeline source must cap to budget, got %+v", got.Candidates)
	}
}

func TestSource_AgentScopedQueryRequestsUncappedProjectionResults(t *testing.T) {
	q := &fixedQuerier{ids: []string{"private-a", "private-b", "visible"}}
	src := NewSource(q)
	plan := domain.QueryPlan{
		Intent: domain.QueryIntent{
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"},
			Kinds:     []domain.FactKind{domain.KindEvent},
			TimeRange: domain.TimeRange{From: time.Unix(1, 0), To: time.Unix(2, 0)},
		},
		SourceBudgets: map[string]int{planner.SourceTimeline: 1},
		TotalCap:      1,
	}

	got := src.Query(context.Background(), plan)
	if q.lastLimit != 0 {
		t.Fatalf("agent-scoped timeline query should defer cap until materialize, got projection limit %d", q.lastLimit)
	}
	if len(got.Candidates) != 3 {
		t.Fatalf("agent-scoped timeline result should not be source-capped, got %+v", got.Candidates)
	}
	if got.Truncated {
		t.Fatal("agent-scoped timeline result should not be marked truncated before materialize")
	}
}
