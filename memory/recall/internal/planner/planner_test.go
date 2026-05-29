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
			name: "relative clause asks for bridge resolution",
			in: port.PlannerInput{
				Text:     "Where did Alice buy the necklace that she wore?",
				Features: domain.QueryFeatures{Tokens: map[string]struct{}{"alice": {}, "buy": {}, "necklace": {}, "wore": {}}},
			},
			want: domain.QueryTaskBridgeResolution,
		},
		{
			name: "multilingual collection surface asks for set completion",
			in: port.PlannerInput{
				Text:     "¿Qué mascotas tiene Jordan?",
				Features: domain.QueryFeatures{Tokens: map[string]struct{}{"mascota": {}, "jordan": {}}},
			},
			want: domain.QueryTaskSetCompletion,
		},
		{
			name: "multilingual bridge surface asks for bridge resolution",
			in: port.PlannerInput{
				Text:     "Où Alice a-t-elle acheté le collier qu'elle portait?",
				Features: domain.QueryFeatures{Tokens: map[string]struct{}{"alice": {}, "collier": {}, "portait": {}}},
			},
			want: domain.QueryTaskBridgeResolution,
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

// TestPlanner_KnownEntitiesInfluenceLensWeights pins planner behavior:
// when the cross-sub-scope KnownEntities merge surfaces an entity
// that also appears in the query (Entities /
// Text / Subject / Object), the rule-based planner emits a small,
// deterministic per-lens weight boost for entity-aware lenses. The
// boost is observable through QueryPlan.LensWeights so the read-path
// plan stage diagnostic surfaces it without changing activation
// rules.
func TestPlanner_KnownEntitiesInfluenceLensWeights(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	baseline, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    scope,
		Entities: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := baseline.LensWeights[SourceEntity]; w != 0 {
		t.Fatalf("baseline LensWeights[entity] = %v, want 0 (no known entities)", w)
	}

	hinted, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    scope,
		Entities: []string{"alice"},
		KnownEntities: []port.EntitySnapshot{
			{Canonical: "alice", Weight: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := hinted.LensWeights[SourceEntity]; w <= 0 {
		t.Fatalf("hinted LensWeights[entity] = %v, want > 0 (known entity intersects query)", w)
	}
	if hinted.LensWeights[SourceRetrieval] != 0 {
		t.Fatalf("retrieval lens should not receive entity-hint boost, got %v", hinted.LensWeights[SourceRetrieval])
	}
	// Boost should be deterministic and match the EntityHintBoost
	// schedule: each matching snapshot contributes Weight × boost.
	want := EntityHintBoost * 2
	if got := hinted.LensWeights[SourceEntity]; got != want {
		t.Fatalf("entity boost = %v, want %v (EntityHintBoost * snapshot.Weight)", got, want)
	}
}

// TestPlanner_KnownEntitiesNoMatchNoBoost guards the "conservative
// boost" contract: when KnownEntities supplies entities that do NOT
// intersect the query, no boost is emitted. This prevents the entity
// hint from drifting into a global "always boost" effect.
func TestPlanner_KnownEntitiesNoMatchNoBoost(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Entities: []string{"bob"},
		KnownEntities: []port.EntitySnapshot{
			{Canonical: "carol"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.LensWeights) != 0 {
		t.Fatalf("LensWeights = %+v, want empty (no query intersection)", plan.LensWeights)
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
