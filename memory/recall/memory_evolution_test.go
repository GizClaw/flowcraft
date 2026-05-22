package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

type captureEvolution struct {
	saves   int
	recalls int
	scope   domain.Scope
	trace   RecallTrace
	saveErr error
}

func (c *captureEvolution) AfterSave(_ context.Context, scope domain.Scope, _ []string) error {
	c.saves++
	c.scope = scope
	return c.saveErr
}

func (c *captureEvolution) AfterRecall(_ context.Context, scope domain.Scope, trace domain.RecallTrace) error {
	c.recalls++
	c.scope = scope
	c.trace = trace
	return nil
}

// TestWithEvolution_HooksSaveAndRecall pins the public hook
// contract: Save and Recall both invoke their respective
// EvolutionRunner methods exactly once with the request's scope.
//
// Cluster F (2026-05-21) note: the trace passed to AfterRecall is
// now a state-derived view (drops only) rather than the full
// diagnostic trace — diagnostics are opt-in via RecallExplain.
// Trace-shape assertions live in TestRecall_WithDiagnostics_*; this
// test only proves the hooks fire with the right scope.
func TestWithEvolution_HooksSaveAndRecall(t *testing.T) {
	ev := &captureEvolution{}
	mem, err := New(WithEvolution(ev))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "hello", Entities: []string{"hello"}}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)
	if _, err := mem.Recall(context.Background(), scope, Query{Entities: []string{"hello"}, Limit: 3}); err != nil {
		t.Fatal(err)
	}
	if ev.saves != 1 || ev.recalls != 1 {
		t.Fatalf("evolution hooks = saves %d recalls %d", ev.saves, ev.recalls)
	}
	if ev.scope.CanonicalKey() != scope.CanonicalKey() {
		t.Fatalf("evolution scope = %+v, want %+v", ev.scope, scope)
	}
}
