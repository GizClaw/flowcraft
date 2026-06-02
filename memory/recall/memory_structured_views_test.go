package recall

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

func TestRecall_StructuredQueryUsesRelationSource(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "alice", Predicate: "spouse", Object: "bob",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Subject:   "alice",
		Predicate: "spouse",
		Object:    "bob",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != res.FactIDs[0] {
		t.Errorf("hits = %+v", hits)
	}
	foundRelation := false
	for _, st := range diagnostics.Sources(trace) {
		if st.Source == planner.SourceRelation && st.Returned > 0 {
			foundRelation = true
		}
	}
	if !foundRelation {
		t.Errorf("relation source should have returned candidates, trace=%+v", diagnostics.Sources(trace))
	}
}

func TestRecall_StructuredQueryCanonicalizesSubjectAndObject(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "Alice", Predicate: "Spouse", Object: "Bob",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Subject:   "alice",
		Predicate: "spouse",
		Object:    "bob",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != res.FactIDs[0] {
		t.Fatalf("structured recall should match canonicalized dimensions, got %+v", hits)
	}
	foundRelation := false
	for _, st := range diagnostics.Sources(trace) {
		if st.Source == planner.SourceRelation && st.Returned > 0 {
			foundRelation = true
		}
	}
	if !foundRelation {
		t.Fatalf("relation source should see canonicalized subject/object, trace=%+v", diagnostics.Sources(trace))
	}
}

func TestRecall_TimelineQueryByTimeRange(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	t0 := time.Unix(1000, 0)
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactEvent, Content: "meeting",
			ObservedAt: t0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, err := mem.Recall(context.Background(), scope, Query{
		TimeRange: TimeRange{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)},
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != res.FactIDs[0] {
		t.Errorf("timeline recall = %+v", hits)
	}
}

func TestRecall_TimelineProjectionRemovesSupersededStateOnSave(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)

	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris",
			ObservedAt: t0,
		}},
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Berlin",
			ObservedAt: t1,
		}},
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		TimeRange: TimeRange{From: t0.Add(-time.Second), To: t1.Add(time.Second)},
		Kinds:     []FactKind{FactState},
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.Content != "Berlin" {
		t.Fatalf("timeline recall should surface only active state, got %+v", hits)
	}
	for _, st := range diagnostics.Sources(trace) {
		if st.Source == planner.SourceTimeline && st.Returned != 1 {
			t.Fatalf("timeline projection should evict superseded state before materialize, trace=%+v", diagnostics.Sources(trace))
		}
	}
	for _, drop := range diagnostics.Drops(trace) {
		if drop.Source == planner.SourceTimeline && drop.Reason == DropSuperseded {
			t.Fatalf("timeline source should not emit superseded candidates after normal Save, drops=%+v", diagnostics.Drops(trace))
		}
	}
}

func TestRecall_PlainQueryDoesNotActivateStructuredSources(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "alice", Predicate: "spouse", Object: "bob",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	_, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Text:  "alice spouse bob",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, st := range diagnostics.Sources(trace) {
		switch st.Source {
		case planner.SourceRelation, planner.SourceProfile, planner.SourceTimeline:
			if st.Returned > 0 {
				t.Errorf("structured source %s should not run on plain query, returned %d", st.Source, st.Returned)
			}
		}
	}
}

func TestRecall_ProfileBySubject(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "nyc",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Subject: "alice",
		Kinds:   []FactKind{FactState},
		Limit:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != res.FactIDs[0] {
		t.Errorf("profile recall = %+v", hits)
	}
	found := false
	for _, st := range diagnostics.Sources(trace) {
		if st.Source == planner.SourceProfile && st.Returned > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("profile source missing from trace: %+v", diagnostics.Sources(trace))
	}
}
