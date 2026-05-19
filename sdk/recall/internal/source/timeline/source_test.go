package timeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

type fixedQuerier struct {
	ids       []string
	lastLimit int
}

func (q *fixedQuerier) Query(_ context.Context, _ model.Scope, _, _ time.Time, _ []model.FactKind, limit int) []string {
	q.lastLimit = limit
	ids := append([]string(nil), q.ids...)
	if limit > 0 && len(ids) > limit {
		return ids[:limit]
	}
	return ids
}

func TestSource_DoesNotMarkExactBudgetAsTruncated(t *testing.T) {
	q := &fixedQuerier{ids: []string{"a", "b"}}
	src := New(q)
	plan := model.QueryPlan{
		Intent: model.QueryIntent{
			Scope:     model.Scope{RuntimeID: "rt", UserID: "u1"},
			Kinds:     []model.FactKind{model.KindEvent},
			TimeRange: model.TimeRange{From: time.Unix(1, 0), To: time.Unix(2, 0)},
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
	src := New(q)
	plan := model.QueryPlan{
		Intent: model.QueryIntent{
			Scope:     model.Scope{RuntimeID: "rt", UserID: "u1"},
			Kinds:     []model.FactKind{model.KindEvent},
			TimeRange: model.TimeRange{From: time.Unix(1, 0), To: time.Unix(2, 0)},
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
