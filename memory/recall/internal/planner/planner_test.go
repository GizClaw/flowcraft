package planner

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestRuleBased_RetrievalOnlyWithoutEntities(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SourceOrder) != 1 || plan.SourceOrder[0] != SourceRetrieval {
		t.Errorf("source order = %+v, want [retrieval]", plan.SourceOrder)
	}
	if plan.Intent.Limit != DefaultLimit {
		t.Errorf("limit defaulting = %d, want %d", plan.Intent.Limit, DefaultLimit)
	}
	if plan.TotalCap != DefaultLimit {
		t.Errorf("total cap = %d, want %d", plan.TotalCap, DefaultLimit)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != DefaultLimit*SourceOverfetchMultiplier {
		t.Errorf("retrieval budget = %d, want overfetch budget %d", got, DefaultLimit*SourceOverfetchMultiplier)
	}
}

func TestRuleBased_EntityActivatedByHints(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Entities: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SourceOrder) != 2 {
		t.Fatalf("source order = %+v", plan.SourceOrder)
	}
	if plan.SourceOrder[0] != SourceRetrieval || plan.SourceOrder[1] != SourceEntity {
		t.Errorf("source order = %+v", plan.SourceOrder)
	}
	if plan.SourceBudgets[SourceEntity] <= 0 {
		t.Errorf("entity budget must be > 0")
	}
}

func TestRuleBased_InfersTaskIntents(t *testing.T) {
	p := New()
	cases := []struct {
		name string
		in   port.PlannerInput
		want domain.QueryTaskIntent
	}{
		{
			name: "count query asks for set completion",
			in: port.PlannerInput{
				Text: "How many books does Alice like?",
				Features: domain.QueryFeatures{
					Tokens:            map[string]struct{}{"alice": {}, "books": {}, "like": {}},
					NumericIntent:     true,
					NumericIntentKind: []domain.QueryNumericIntentKind{domain.QueryNumericIntentCount},
				},
			},
			want: domain.QueryTaskSetCompletion,
		},
		{
			name: "temporal query asks for temporal reasoning",
			in: port.PlannerInput{
				Text: "When did Alice move?",
				Features: domain.QueryFeatures{Temporal: domain.QueryTemporalFeatures{
					HasIntent: true,
					IntentKind: []domain.QueryTemporalIntentKind{
						domain.QueryTemporalIntentDate,
					},
				}},
			},
			want: domain.QueryTaskTemporalReasoning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Scope = domain.Scope{RuntimeID: "rt"}
			plan, err := p.Plan(context.Background(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !containsTask(plan.TaskIntents, tc.want) {
				t.Fatalf("TaskIntents = %+v, want %s", plan.TaskIntents, tc.want)
			}
		})
	}
}

func TestRuleBased_SurfaceIntentHelpersRespectTokenBoundaries(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Text:     "The theater was busy.",
		Features: domain.QueryFeatures{Tokens: map[string]struct{}{"theater": {}, "busy": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsTask(plan.TaskIntents, domain.QueryTaskBridgeResolution) {
		t.Fatalf("TaskIntents = %+v, theater must not trigger her bridge cue", plan.TaskIntents)
	}
}

func containsTask(tasks []domain.QueryTaskIntent, want domain.QueryTaskIntent) bool {
	for _, task := range tasks {
		if task == want {
			return true
		}
	}
	return false
}

func TestActivatesTimelineForDirectDateIntent(t *testing.T) {
	intent := domain.QueryIntent{
		Kinds: []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan},
		Features: domain.QueryFeatures{Temporal: domain.QueryTemporalFeatures{
			HasIntent:  true,
			IntentKind: []domain.QueryTemporalIntentKind{domain.QueryTemporalIntentDate},
		}},
	}

	if !ActivatesTimeline(intent) {
		t.Fatalf("timeline should activate for direct date/when intent")
	}
}

func TestActivatesTimelineSkipsBroadTemporalIntentWithoutRange(t *testing.T) {
	intent := domain.QueryIntent{
		Kinds: []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan},
		Features: domain.QueryFeatures{Temporal: domain.QueryTemporalFeatures{
			HasIntent:  true,
			IntentKind: []domain.QueryTemporalIntentKind{domain.QueryTemporalIntentDate, domain.QueryTemporalIntentRange},
		}},
	}

	if ActivatesTimeline(intent) {
		t.Fatalf("timeline should not activate for broad range intent without explicit time bounds")
	}
}

func TestActivatesTimelinePreservesExplicitStructuredKinds(t *testing.T) {
	intent := domain.QueryIntent{
		Kinds: []domain.FactKind{domain.KindEvent},
	}

	if !ActivatesTimeline(intent) {
		t.Fatalf("timeline should still activate for explicit structured temporal kinds")
	}
}

func TestRuleBased_SourceBudgetsOverfetchFinalLimit(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Entities: []string{"alice"},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != 20 {
		t.Errorf("retrieval budget = %d, want 20", got)
	}
	if got := plan.SourceBudgets[SourceEntity]; got != 20 {
		t.Errorf("entity budget = %d, want 20", got)
	}
	if plan.TotalCap != 10 {
		t.Errorf("total cap = %d, want 10", plan.TotalCap)
	}
}

func TestRuleBased_SourceBudgetCapsAtMaxOverfetch(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Limit: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != MaxSourceOverfetch {
		t.Errorf("retrieval budget = %d, want %d", got, MaxSourceOverfetch)
	}
	if plan.TotalCap != 30 {
		t.Errorf("total cap = %d, want 30", plan.TotalCap)
	}
}

func TestFusionCandidateCapKeepsWiderCrossSourcePool(t *testing.T) {
	if got := FusionCandidateCap(30); got != 90 {
		t.Fatalf("fusion candidate cap = %d, want 90", got)
	}
	if got := FusionCandidateCap(50); got != MaxFusionCandidateCap {
		t.Fatalf("fusion candidate cap = %d, want max %d", got, MaxFusionCandidateCap)
	}
}

func TestPlanner_KnownEntitiesDoNotInfluenceLensWeights(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    scope,
		Entities: []string{"alice"},
		KnownEntities: []port.EntitySnapshot{
			{Canonical: "alice", Weight: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.LensWeights) != 0 {
		t.Fatalf("LensWeights = %+v, want empty; KnownEntities should not boost lens weights", plan.LensWeights)
	}
}

func TestRuleBased_ClampsMaxLimit(t *testing.T) {
	p := New()
	plan, _ := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Limit: MaxLimit + 50,
	})
	if plan.Intent.Limit != MaxLimit {
		t.Errorf("limit = %d, want clamped to %d", plan.Intent.Limit, MaxLimit)
	}
	if plan.TotalCap != MaxLimit {
		t.Errorf("total cap = %d", plan.TotalCap)
	}
}
