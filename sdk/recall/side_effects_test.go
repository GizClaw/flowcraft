package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

type hookUnderLockDetector struct {
	telemetry.NopHook
	mem    *memory
	scope  Scope
	mu     sync.Mutex
	events int
}

func (h *hookUnderLockDetector) OnStage(_ diagnostic.StageDiagnostic) {
	if h.mem == nil {
		return
	}
	acquired := make(chan struct{})
	go func() {
		unlock := h.mem.lockWriteScope(h.scope)
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return
	case <-time.After(10 * time.Millisecond):
		h.mu.Lock()
		h.events++
		h.mu.Unlock()
	}
}

func TestSave_SideEffectsDrainAfterUnlock(t *testing.T) {
	hook := &hookUnderLockDetector{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		WithAsyncSemanticQueue(asyncsemantic.New()),
		WithTelemetryHook(hook),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	hook.mem = mem.(*memory)
	hook.scope = scope
	_, err = mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		Kind: domain.KindState, Subject: "a", Predicate: "b", Content: "c",
	}}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.events != 0 {
		t.Fatalf("telemetry must not run under scope lock, got %d OnStage calls", hook.events)
	}
}

func TestProcessSideEffects_DeadLettersAfterMaxAttempts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		withExtraProjection(failingProjection{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "will fail projection"}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	now := time.Now()
	var out SideEffectProcessResult
	for i := 0; i < maxSideEffectAttempts+3; i++ {
		out, err = proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{
			Scope: scope,
			Limit: 1,
			Now:   now.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.DeadLetter > 0 {
			break
		}
	}
	if out.DeadLetter != 1 || out.Failed != 1 {
		t.Fatalf("final process result = %+v, want dead-letter failure", out)
	}
	obs := mem.(SideEffectOutboxObserver)
	stats, err := obs.SideEffectOutboxStats(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetter != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want dead-letter", stats)
	}
}

func TestProcessSideEffects_EvolutionFailureRetries(t *testing.T) {
	ev := &captureEvolution{saveErr: errors.New("evolution down")}
	mem, err := New(WithEvolution(ev))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "evolve me"}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{
		Scope: scope,
		Limit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Fatalf("process result = %+v, want evolution failure", out)
	}
	stats, err := mem.(SideEffectOutboxObserver).SideEffectOutboxStats(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("stats = %+v, want pending retry for evolution", stats)
	}
}

func TestProcessSideEffects_ReturnsCompleteAckError(t *testing.T) {
	mem, err := New(WithSideEffectOutbox(ackFailOutbox{
		inner: NewInMemorySideEffectOutbox(),
		fail:  "complete",
	}))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "ack complete"}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{Scope: scope, Limit: 1})
	if err == nil {
		t.Fatal("complete ack error must be returned")
	}
	if out.Failed != 1 {
		t.Fatalf("result = %+v, want failed=1", out)
	}
}

func TestProcessSideEffects_ReturnsFailAckError(t *testing.T) {
	mem, err := New(
		WithSideEffectOutbox(ackFailOutbox{
			inner: NewInMemorySideEffectOutbox(),
			fail:  "fail",
		}),
		withExtraProjection(failingProjection{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "ack fail"}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{Scope: scope, Limit: 1})
	if err == nil {
		t.Fatal("fail ack error must be returned")
	}
	if out.Failed != 1 {
		t.Fatalf("result = %+v, want failed=1", out)
	}
}

type ackFailOutbox struct {
	inner port.SideEffectOutbox
	fail  string
}

func (o ackFailOutbox) Enqueue(ctx context.Context, job port.SideEffectJob) error {
	return o.inner.Enqueue(ctx, job)
}

func (o ackFailOutbox) Claim(ctx context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	return o.inner.Claim(ctx, opts)
}

func (o ackFailOutbox) Complete(ctx context.Context, jobID, leaseToken string, result port.SideEffectResult) error {
	if o.fail == "complete" {
		return fmt.Errorf("complete ack down")
	}
	return o.inner.Complete(ctx, jobID, leaseToken, result)
}

func (o ackFailOutbox) Fail(ctx context.Context, jobID, leaseToken string, failure port.SideEffectFailure) error {
	if o.fail == "fail" {
		return fmt.Errorf("fail ack down")
	}
	return o.inner.Fail(ctx, jobID, leaseToken, failure)
}

func (o ackFailOutbox) Cancel(ctx context.Context, requestID string) error {
	return o.inner.Cancel(ctx, requestID)
}

func (o ackFailOutbox) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	return o.inner.CancelScope(ctx, scope)
}

func (o ackFailOutbox) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	return o.inner.PurgeScope(ctx, scope)
}

func (o ackFailOutbox) Stats(ctx context.Context, scope domain.Scope, now time.Time) (port.SideEffectStats, error) {
	return o.inner.Stats(ctx, scope, now)
}
