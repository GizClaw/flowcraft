package typesafe

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

// TestConformanceDecisionUnary drives the shared unary suite over a scripted
// route: Explain must compile without provider I/O, and Execute must read one
// round trip into a response whose usage is stamped with the model the caller
// resolved.
func TestConformanceDecisionUnary(t *testing.T) {
	route := &fakeRoute{body: wireResponseFixture()}
	cls := testClients(t, route, Spec{})

	var calls atomic.Int64
	driver, err := inference.BindDecision(
		compileDecision(DefaultModel),
		countingTransport(&calls, transportDecision(cls)),
		decodeDecision,
	)
	if err != nil {
		t.Fatalf("BindDecision: %v", err)
	}
	inferencetest.RunDecisionUnary(t, inferencetest.DecisionUnarySuite{
		Model:   typesafeModel(DefaultModel),
		Request: decisionRequestFixture,
		Driver:  driver,
		TransportCalls: func() int64 {
			return calls.Load()
		},
	})
}

// TestConformanceDecisionUnaryExecuteIsStable checks the attempt a caller
// repeats: the same compiled request executed twice reaches the route twice and
// decodes into the same canonical answer set, because the driver keeps no state
// between calls — the runtime caches bound drivers and calls them concurrently.
func TestConformanceDecisionUnaryExecuteIsStable(t *testing.T) {
	route := &fakeRoute{body: wireResponseFixture()}
	cls := testClients(t, route, Spec{})
	driver, err := inference.BindDecision(
		compileDecision(DefaultModel),
		transportDecision(cls),
		decodeDecision,
	)
	if err != nil {
		t.Fatalf("BindDecision: %v", err)
	}

	first, err := driver.Execute(
		context.Background(), typesafeModel(DefaultModel), decisionRequestFixture())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	second, err := driver.Execute(
		context.Background(), typesafeModel(DefaultModel), decisionRequestFixture())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(first.Answers) != len(second.Answers) || len(first.Answers) != 3 {
		t.Fatalf("answers = %d and %d", len(first.Answers), len(second.Answers))
	}
	for index := range first.Answers {
		if first.Answers[index].Name != second.Answers[index].Name {
			t.Errorf("answer %d = %q then %q",
				index, first.Answers[index].Name, second.Answers[index].Name)
		}
	}
	if route.callCount() != 2 {
		t.Errorf("route saw %d calls, want one per execute", route.callCount())
	}
}
