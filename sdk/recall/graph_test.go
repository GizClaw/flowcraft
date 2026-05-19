package recall

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

func TestRecall_GraphDisabledByDefault(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	_, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(),
		Scope{RuntimeID: "rt", UserID: "u1"},
		Query{Entities: []string{"alice"}, Limit: 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range trace.Sources {
		if st.Source == planner.SourceGraph {
			t.Fatalf("graph source must not run by default, trace=%+v", trace.Sources)
		}
	}
}

func TestRecall_GraphExpansionMultiHop(t *testing.T) {
	mem, err := New(WithGraphEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactRelation, Subject: "alice", Predicate: "friend", Object: "bob"},
			{Kind: FactRelation, Subject: "bob", Predicate: "friend", Object: "charlie"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Entities:  []string{"alice"},
		GraphHops: 2,
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("graph expansion should surface chained relations, hits=%+v", hits)
	}
	foundGraph := false
	for _, st := range trace.Sources {
		if st.Source == planner.SourceGraph && st.Returned > 0 {
			foundGraph = true
		}
	}
	if !foundGraph {
		t.Fatalf("graph source should return candidates, trace=%+v", trace.Sources)
	}
}

func TestRecall_GraphDoesNotTraverseOtherAgentPrivateEdges(t *testing.T) {
	mem, err := New(WithGraphEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	shared := Scope{RuntimeID: "rt", UserID: "u1"}
	agentA := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	agentB := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}

	if _, err := mem.Save(context.Background(), agentB, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "alice", Predicate: "knows", Object: "bob",
			Content: "agent-b private bridge",
		}},
	}); err != nil {
		t.Fatalf("save private bridge: %v", err)
	}
	sharedRes, err := mem.Save(context.Background(), shared, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "bob", Predicate: "knows", Object: "carol",
			Content: "shared fact beyond private bridge",
		}},
	})
	if err != nil {
		t.Fatalf("save shared edge: %v", err)
	}

	hits, _, err := mem.(RecallExplainer).RecallExplain(context.Background(), agentA, Query{
		Entities:  []string{"alice"},
		GraphHops: 2,
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsFactID(hits, sharedRes.FactIDs[0]) {
		t.Fatalf("agent-a graph recall must not discover shared facts through agent-b private edges, hits=%+v", hits)
	}
}

func TestRecall_GraphProjectionRemovesSupersededEdgesOnSave(t *testing.T) {
	mem, err := New(WithGraphEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris",
			Entities: []string{"alice", "paris"}, ObservedAt: t0,
		}},
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Berlin",
			Entities: []string{"alice", "berlin"}, ObservedAt: t1,
		}},
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	_, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Entities:  []string{"paris"},
		GraphHops: 1,
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range trace.Sources {
		if st.Source == planner.SourceGraph && st.Returned != 0 {
			t.Fatalf("graph projection should evict superseded edges before materialize, trace=%+v", trace.Sources)
		}
	}
	for _, drop := range trace.Drops {
		if drop.Source == planner.SourceGraph && drop.FactID == first.FactIDs[0] && drop.Reason == DropSuperseded {
			t.Fatalf("graph source should not emit superseded edge candidates after normal Save, drops=%+v", trace.Drops)
		}
	}
}

func TestRecall_GraphHopsIsCappedByDefaultBound(t *testing.T) {
	mem, err := New(WithGraphEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	third, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactRelation, Subject: "alice", Predicate: "knows", Object: "bob"},
			{Kind: FactRelation, Subject: "bob", Predicate: "knows", Object: "carol"},
			{Kind: FactRelation, Subject: "carol", Predicate: "knows", Object: "dave"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits, _, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Entities:  []string{"alice"},
		GraphHops: 999,
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsFactID(hits, third.FactIDs[2]) {
		t.Fatalf("GraphHops must be capped by the default bounded expansion, hits=%+v", hits)
	}
}

func containsFactID(hits []Hit, factID string) bool {
	for _, h := range hits {
		if h.Fact.ID == factID {
			return true
		}
	}
	return false
}
