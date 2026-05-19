package timeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

func scope() model.Scope { return model.Scope{RuntimeID: "rt", UserID: "u1"} }

func TestTimeline_KeepsPastEventWithValidTo(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Unix(100, 0)
	validTo := time.Unix(200, 0)
	f := model.TemporalFact{
		ID: "ev1", Scope: scope(), Kind: model.KindEvent,
		ObservedAt: past, ValidTo: &validTo,
	}
	if err := p.Project(ctx, []model.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0)
	if len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("past event with ValidTo must remain in temporal view, got %+v", got)
	}
}

func TestTimeline_DropsSuperseded(t *testing.T) {
	p := New()
	ctx := context.Background()
	f := model.TemporalFact{
		ID: "old", Scope: scope(), Kind: model.KindState,
		Subject: "alice", Predicate: "city", Content: "nyc",
		ObservedAt: time.Unix(1, 0), CorrectedBy: "new",
	}
	if err := p.Project(ctx, []model.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0); len(got) != 0 {
		t.Fatalf("superseded fact must not appear, got %+v", got)
	}
}

func TestTimeline_RangeScan(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []model.TemporalFact{
		{ID: "a", Scope: scope(), Kind: model.KindEvent, ObservedAt: time.Unix(10, 0)},
		{ID: "b", Scope: scope(), Kind: model.KindPlan, ObservedAt: time.Unix(20, 0)},
		{ID: "c", Scope: scope(), Kind: model.KindNote, ObservedAt: time.Unix(15, 0)},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Unix(9, 0), time.Unix(21, 0), nil, 0)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("range scan = %+v, want [a b]", got)
	}
}

func TestTimeline_RebuildExactReplace(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "stale", Scope: scope(), Kind: model.KindEvent, ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []model.TemporalFact{
		{ID: "fresh", Scope: scope(), Kind: model.KindEvent, ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0)
	if len(got) != 1 || got[0] != "fresh" {
		t.Errorf("rebuild must exact-replace, got %+v", got)
	}
}
