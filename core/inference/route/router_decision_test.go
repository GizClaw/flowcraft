package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// decisionRouteRequest is the minimal valid decision request for selector
// tests: one predicate question, plus the model hint under test.
func decisionRouteRequest(hint string) inference.DecisionRequest {
	return inference.DecisionRequest{
		Input: inference.DecisionInput{Text: "the evidence"},
		Questions: []inference.DecisionQuestion{{
			Name:         "is_true",
			Kind:         inference.DecisionQuestionPredicate,
			Instructions: "is the statement true?",
		}},
		ModelHint: hint,
	}
}

// TestPolicyDecisionHintSelectsConfiguredTarget pins the decision selector's
// model_hint branch: an exact provider/name hint wins over the head of the
// pool, and the decision reports the hinted target's tier.
func TestPolicyDecisionHintSelectsConfiguredTarget(t *testing.T) {
	good := model.ModelRef{
		ID:      model.ModelID{Provider: "good", Name: "model-1"},
		Profile: "prod",
	}
	policy := Policy{Decision: []Pool{
		{Tier: "primary", Targets: []Target{{Model: model.ModelRef{
			ID: model.ModelID{Provider: "bad", Name: "model-1"},
		}}}},
		{Tier: "fallback", Targets: []Target{{Model: good}}},
	}}
	selectors := policy.Selectors(nil)
	decision, err := selectors.Decision.SelectDecision(
		context.Background(),
		decisionRouteRequest("good/model-1"),
	)
	if err != nil {
		t.Fatalf("SelectDecision: %v", err)
	}
	if decision.Selected != good {
		t.Fatalf("Selected = %+v, want hinted target %+v",
			decision.Selected, good)
	}
	if decision.Tier != "fallback" {
		t.Fatalf("Tier = %q, want hinted target's tier", decision.Tier)
	}
	if decision.Operation != model.OperationDecision {
		t.Fatalf("Operation = %q, want %q",
			decision.Operation, model.OperationDecision)
	}
}

// TestPolicyDecisionHintUnknownOrEmptyFallsBack pins the other half of the
// hint branch: an absent, unknown, or malformed hint leaves the declared
// order in charge.
func TestPolicyDecisionHintUnknownOrEmptyFallsBack(t *testing.T) {
	first := model.ModelRef{
		ID: model.ModelID{Provider: "bad", Name: "model-1"},
	}
	policy := Policy{Decision: []Pool{
		{Tier: "primary", Targets: []Target{{Model: first}}},
		{Tier: "fallback", Targets: []Target{{Model: model.ModelRef{
			ID: model.ModelID{Provider: "good", Name: "model-1"},
		}}}},
	}}
	selectors := policy.Selectors(nil)
	for _, hint := range []string{
		"", "nope/model-1", "bad/model-2", "/model-1", "bad/", "a/b/c",
	} {
		decision, err := selectors.Decision.SelectDecision(
			context.Background(),
			decisionRouteRequest(hint),
		)
		if err != nil {
			t.Fatalf("hint %q: SelectDecision: %v", hint, err)
		}
		if decision.Selected != first {
			t.Fatalf("hint %q: Selected = %+v, want default %+v",
				hint, decision.Selected, first)
		}
	}
}

func TestPolicyDecisionWithoutPoolsReportsNoRoute(t *testing.T) {
	selectors := Policy{}.Selectors(nil)
	_, err := selectors.Decision.SelectDecision(
		context.Background(),
		decisionRouteRequest(""),
	)
	if !IsKind(err, NoRoute) {
		t.Fatalf("SelectDecision error = %v, want NoRoute", err)
	}
}

// TestPolicyDecisionFallbackKeepsHintElevation pins the fallback order for a
// hinted decision call: the hinted target runs first and is never revisited,
// and the remaining chain keeps its declared order.
func TestPolicyDecisionFallbackKeepsHintElevation(t *testing.T) {
	first := model.ModelRef{
		ID: model.ModelID{Provider: "first", Name: "model-1"},
	}
	second := model.ModelRef{
		ID: model.ModelID{Provider: "second", Name: "model-1"},
	}
	hinted := model.ModelRef{
		ID: model.ModelID{Provider: "hinted", Name: "model-1"},
	}
	policy := Policy{Decision: []Pool{
		{Tier: "primary", Targets: []Target{
			{Model: first}, {Model: second},
		}},
		{Tier: "late", Targets: []Target{{Model: hinted}}},
	}}
	selectors := policy.Selectors(nil)
	request := decisionRouteRequest("hinted/model-1")
	steps := []struct {
		attempt model.ModelRef
		want    model.ModelRef
		ok      bool
	}{
		{attempt: hinted, want: first, ok: true},
		{attempt: first, want: second, ok: true},
		{attempt: second, ok: false},
	}
	for _, step := range steps {
		next, ok, err := selectors.DecisionFallback.NextDecision(
			context.Background(),
			request,
			Attempt{Target: step.attempt},
		)
		if err != nil {
			t.Fatalf("NextDecision after %+v: %v", step.attempt, err)
		}
		if ok != step.ok {
			t.Fatalf("NextDecision after %+v = %+v, %v, want ok=%v",
				step.attempt, next, ok, step.ok)
		}
		if ok && next != step.want {
			t.Fatalf("NextDecision after %+v = %+v, want %+v",
				step.attempt, next, step.want)
		}
	}
}
