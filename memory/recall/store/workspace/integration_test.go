package workspace

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
)

func TestBackendIntegratesWithMemoryReadiness(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	mem, err := recall.New(
		recall.WithTemporalStore(b.TemporalStore()),
		recall.WithSideEffectOutbox(b.SideEffectOutbox()),
		recall.WithAsyncSemanticQueue(b.AsyncSemanticQueue()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(ctx, scope, recall.SaveRequest{
		Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := mem.(recall.ReadinessObserver).Readiness(ctx, scope, recall.ReadinessOptions{
		MaxSideEffectBacklog: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != recall.ReadinessReady {
		t.Fatalf("readiness status = %s, want ready: %+v", report.Status, report)
	}

	proc, ok := recall.NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("side-effect processor unavailable")
	}
	res, err := proc.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed == 0 {
		t.Fatalf("side-effect process result = %+v, want completed jobs", res)
	}
}
