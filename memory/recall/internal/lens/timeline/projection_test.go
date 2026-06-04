package timeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func TestTimeline_AcceptsOnlyTimelineKinds(t *testing.T) {
	p := New()
	for _, kind := range []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan} {
		if !p.AcceptsKind(kind) {
			t.Fatalf("AcceptsKind(%q) = false, want true", kind)
		}
	}
	if p.AcceptsKind(domain.KindParameter) {
		t.Fatal("timeline must not advertise KindParameter eligibility")
	}
}

func TestTimeline_KeepsPastEventWithOpenValidity(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	validTo := time.Now().Add(time.Hour)
	f := domain.TemporalFact{
		ID: "ev1", Scope: scope(), Kind: domain.KindEvent,
		ObservedAt: past, ValidTo: &validTo,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0)
	if len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("past event with open ValidTo must remain in temporal view, got %+v", got)
	}
}

// TestTimeline_KeepsPastEventWithClosedValidity pins the historical-
// view invariant restored alongside domain.IsHistorical: a fact whose
// validity window has long closed (e.g. a one-day event in 2023 viewed
// from 2026) must STILL be indexed by the timeline. An earlier
// IsProjectable adoption silently broke this — historical
// "When did X happen?" queries lost the underlying event because
// IsCanonicalActive returns false once ValidTo < now.
func TestTimeline_KeepsPastEventWithClosedValidity(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Now().Add(-365 * 24 * time.Hour)
	closed := time.Now().Add(-30 * 24 * time.Hour)
	f := domain.TemporalFact{
		ID: "ev1", Scope: scope(), Kind: domain.KindEvent,
		ObservedAt: past, ValidTo: &closed,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0)
	if len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("past event with closed ValidTo must still appear in timeline (historical view), got %+v", got)
	}
}

func TestTimeline_DropsSuperseded(t *testing.T) {
	p := New()
	ctx := context.Background()
	f := domain.TemporalFact{
		ID: "old", Scope: scope(), Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "nyc",
		ObservedAt: time.Unix(1, 0), CorrectedBy: "new",
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0); len(got) != 0 {
		t.Fatalf("superseded fact must not appear, got %+v", got)
	}
}

func TestTimeline_RangeScan(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []domain.TemporalFact{
		{ID: "a", Scope: scope(), Kind: domain.KindEvent, ObservedAt: time.Unix(10, 0)},
		{ID: "b", Scope: scope(), Kind: domain.KindPlan, ObservedAt: time.Unix(20, 0)},
		{ID: "c", Scope: scope(), Kind: domain.KindNote, ObservedAt: time.Unix(15, 0)},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Unix(9, 0), time.Unix(21, 0), nil, 0)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("range scan = %+v, want [a b]", got)
	}
}

// TestTimeline_DropsClosed pins that a soft-forgotten (Closed) fact
// must not survive in the timeline projection cache. Before the
// predicate split, timeline inline-checked only CorrectedBy and
// happily indexed Closed facts.
func TestTimeline_DropsClosed(t *testing.T) {
	p := New()
	ctx := context.Background()
	f := domain.TemporalFact{
		ID: "ev1", Scope: scope(), Kind: domain.KindEvent,
		ObservedAt: time.Now().Add(-time.Hour), Closed: true,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0); len(got) != 0 {
		t.Fatalf("Closed fact must not be indexed by timeline, got %+v", got)
	}
}

func TestTimeline_RebuildExactReplace(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "stale", Scope: scope(), Kind: domain.KindEvent, ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []domain.TemporalFact{
		{ID: "fresh", Scope: scope(), Kind: domain.KindEvent, ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	got := p.Query(ctx, scope(), time.Time{}, time.Time{}, nil, 0)
	if len(got) != 1 || got[0] != "fresh" {
		t.Errorf("rebuild must exact-replace, got %+v", got)
	}
}
