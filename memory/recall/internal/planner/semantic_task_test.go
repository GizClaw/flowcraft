package planner

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestRuleBasedPlannerDoesNotInferSemanticTasksFromCues(t *testing.T) {
	plan, err := New().Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "Did Dave cancel the Dodge Charger test drive?",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"dave": {}, "cancel": {}, "dodge": {}, "charger": {}, "test": {}, "drive": {}},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoTask(t, plan.TaskIntents, domain.QueryTaskYesNoVerification)
	assertNoTask(t, plan.TaskIntents, domain.QueryTaskAbsenceCheck)
	assertNoSource(t, plan.SourceOrder, SourceAssertion)
}

func TestRuleBasedPlannerDoesNotInferCounterfactualTaskFromCue(t *testing.T) {
	plan, err := New().Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "Would Mira have moved if the lease had been cheaper?",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"mira": {}, "moved": {}, "lease": {}, "cheaper": {}},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoTask(t, plan.TaskIntents, domain.QueryTaskCounterfactual)
	assertNoSource(t, plan.SourceOrder, SourceAssertion)
}

func TestRuleBasedPlannerOrdinaryIfQuestionIsNotCounterfactual(t *testing.T) {
	plan, err := New().Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "Would Mira move if the lease is cheaper?",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"mira": {}, "move": {}, "lease": {}, "cheaper": {}},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoTask(t, plan.TaskIntents, domain.QueryTaskCounterfactual)
}

func TestRuleBasedPlannerDoesNotTreatEveryQuestionAsYesNo(t *testing.T) {
	plan, err := New().Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "What city did Mira visit?",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"city": {}, "mira": {}, "visit": {}},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoTask(t, plan.TaskIntents, domain.QueryTaskYesNoVerification)
	assertNoSource(t, plan.SourceOrder, SourceAssertion)
}

func TestRuleBasedPlannerBareWhichDoesNotActivateAssertion(t *testing.T) {
	plan, err := New().Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "Which city did Mira visit?",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"city": {}, "mira": {}, "visit": {}},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoSource(t, plan.SourceOrder, SourceAssertion)
}

func assertNoTask(t *testing.T, got []domain.QueryTaskIntent, want domain.QueryTaskIntent) {
	t.Helper()
	for _, task := range got {
		if task == want {
			t.Fatalf("unexpected task %q in %v", want, got)
		}
	}
}

func assertNoSource(t *testing.T, got []string, want string) {
	t.Helper()
	for _, source := range got {
		if source == want {
			t.Fatalf("unexpected source %q in %v", want, got)
		}
	}
}
