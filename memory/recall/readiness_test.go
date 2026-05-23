package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestReadiness_ReportsReadyWhenNoBacklog(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	report, err := mem.(ReadinessObserver).Readiness(context.Background(), Scope{RuntimeID: "rt", UserID: "u1"}, ReadinessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != ReadinessReady {
		t.Fatalf("status = %s, want ready: %+v", report.Status, report)
	}
	if got := readinessCheck(report, "async_semantic_queue"); got.Status != ReadinessSkipped {
		t.Fatalf("async queue check = %+v, want skipped", got)
	}
}

func TestReadiness_DegradesOnSideEffectBacklog(t *testing.T) {
	ctx := context.Background()
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}}}); err != nil {
		t.Fatal(err)
	}

	report, err := mem.(ReadinessObserver).Readiness(ctx, scope, ReadinessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != ReadinessDegraded {
		t.Fatalf("status = %s, want degraded: %+v", report.Status, report)
	}
	check := readinessCheck(report, "side_effect_outbox")
	if check.Backlog == 0 || check.Status != ReadinessDegraded {
		t.Fatalf("side-effect check = %+v, want degraded backlog", check)
	}
}

func TestReadiness_RequireAsyncSemanticQueue(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	report, err := mem.(ReadinessObserver).Readiness(context.Background(), Scope{RuntimeID: "rt", UserID: "u1"}, ReadinessOptions{
		RequireAsyncSemantic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != ReadinessNotReady {
		t.Fatalf("status = %s, want not_ready: %+v", report.Status, report)
	}
	if got := readinessCheck(report, "async_semantic_queue"); got.Status != ReadinessNotReady {
		t.Fatalf("async queue check = %+v, want not_ready", got)
	}
}

func TestReadiness_IncludesAsyncSemanticReconcile(t *testing.T) {
	ctx := context.Background()
	queue := asyncsemantic.New()
	mem, err := New(WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "I like tea"}},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := mem.(ReadinessObserver).Readiness(ctx, scope, ReadinessOptions{
		IncludeAsyncSemanticReconcile: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := readinessCheck(report, "async_semantic_reconcile")
	if check.PendingSemantic != 1 || check.Status != ReadinessDegraded {
		t.Fatalf("async reconcile check = %+v, want one pending degraded request", check)
	}
}

func TestReadiness_Validation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	_, err = mem.(ReadinessObserver).Readiness(context.Background(), Scope{}, ReadinessOptions{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func readinessCheck(report ReadinessReport, name string) ReadinessCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return ReadinessCheck{}
}
