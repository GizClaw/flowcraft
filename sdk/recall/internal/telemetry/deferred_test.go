package telemetry_test

import (
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

type captureHook struct {
	mu     sync.Mutex
	events []diagnostic.StageDiagnostic
}

func (h *captureHook) OnStage(d diagnostic.StageDiagnostic) {
	h.mu.Lock()
	h.events = append(h.events, d)
	h.mu.Unlock()
}

func TestDeferred_HoldBuffersUntilFlush(t *testing.T) {
	inner := &captureHook{}
	d := telemetry.NewDeferred(inner)
	d.Hold()
	d.OnStage(diagnostic.StageDiagnostic{Stage: "a"})
	if len(inner.events) != 0 {
		t.Fatalf("expected buffered, got %d events", len(inner.events))
	}
	d.Flush()
	if len(inner.events) != 1 || inner.events[0].Stage != "a" {
		t.Fatalf("after flush = %+v", inner.events)
	}
	d.OnStage(diagnostic.StageDiagnostic{Stage: "b"})
	if len(inner.events) != 2 {
		t.Fatalf("passthrough after flush = %+v", inner.events)
	}
}

func TestDeferred_NestedHoldRequiresMatchingFlushDepth(t *testing.T) {
	inner := &captureHook{}
	d := telemetry.NewDeferred(inner)
	d.Hold()
	d.OnStage(diagnostic.StageDiagnostic{Stage: "a"})
	d.Hold()
	d.OnStage(diagnostic.StageDiagnostic{Stage: "b"})
	d.Flush()
	if len(inner.events) != 0 {
		t.Fatalf("inner flush after first Flush, got %+v", inner.events)
	}
	d.Flush()
	if len(inner.events) != 2 {
		t.Fatalf("want both events after final flush, got %+v", inner.events)
	}
}

func TestDeferred_ConcurrentHoldsDoNotPrematurelyPassthrough(t *testing.T) {
	inner := &captureHook{}
	d := telemetry.NewDeferred(inner)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.Hold()
		d.OnStage(diagnostic.StageDiagnostic{Stage: "a"})
		d.Flush()
	}()
	go func() {
		defer wg.Done()
		d.Hold()
		d.OnStage(diagnostic.StageDiagnostic{Stage: "b"})
		d.Flush()
	}()
	wg.Wait()
	if len(inner.events) != 2 {
		t.Fatalf("expected 2 flushed events, got %+v", inner.events)
	}
}
