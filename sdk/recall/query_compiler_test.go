package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

func TestRecall_QueryCompilerActivatesEntityAndGraphFromText(t *testing.T) {
	mem, err := New(WithGraphEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactRelation, Subject: "alice", Predicate: "met", Object: "bob", Entities: []string{"alice", "bob"}},
			{Kind: FactState, Subject: "alice", Predicate: "city", Object: "paris", Entities: []string{"alice", "paris"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Text:  "Who did Alice meet in Paris?",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Plan.Intent.Entities) == 0 {
		t.Fatalf("query compiler should extract entities, intent=%+v", trace.Plan.Intent)
	}
	gotEntity, gotGraph := false, false
	for _, src := range trace.Sources {
		switch src.Source {
		case planner.SourceEntity:
			gotEntity = true
		case planner.SourceGraph:
			gotGraph = true
		}
	}
	if !gotEntity {
		t.Fatalf("entity source should run for extracted seeds, sources=%+v", trace.Sources)
	}
	if !gotGraph {
		t.Fatalf("graph source should run when graph enabled and entities extracted, sources=%+v", trace.Sources)
	}
}
