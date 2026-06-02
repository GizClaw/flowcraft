package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/internal/telemetry"
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

func TestProcessSideEffects_SkipsStaleGenerationJobs(t *testing.T) {
	mem, err := New(withExtraProjection(failingProjection{}))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "stale projection"}},
	}); err != nil {
		t.Fatal(err)
	}
	m := mem.(*memory)
	m.bumpScopeGen(scope)

	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{Scope: scope, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out.Completed == 0 || out.Failed != 0 {
		t.Fatalf("stale side-effect job should be completed without executing projection, got %+v", out)
	}
	stats, err := mem.(SideEffectOutboxObserver).SideEffectOutboxStats(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Completed != out.Completed || stats.Pending != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want stale job completed", stats)
	}
}

func TestProcessSideEffects_SkipsDeletedFactJobs(t *testing.T) {
	mem, err := New(withExtraProjection(failingProjection{}))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "delete before side effect"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("Save fact ids = %+v", res.FactIDs)
	}
	if err := mem.Forget(context.Background(), scope, res.FactIDs[0], ForgetHard); err != nil {
		t.Fatal(err)
	}

	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{Scope: scope, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out.Completed == 0 || out.Failed != 0 {
		t.Fatalf("deleted fact side-effect jobs should complete without projection, got %+v", out)
	}
}

func TestProcessSideEffects_DetachesCompleteAckAfterExecution(t *testing.T) {
	base := NewInMemorySideEffectOutbox()
	outbox := ctxAwareCompleteOutbox{inner: base}
	ctx, cancel := context.WithCancel(context.Background())
	mem, err := New(
		WithSideEffectOutbox(outbox),
		withExtraProjection(cancelingProjection{cancel: cancel}),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "cancel after project"}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	out, err := proc.ProcessSideEffects(ctx, SideEffectProcessOptions{Scope: scope, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out.Completed == 0 || out.Failed != 0 {
		t.Fatalf("complete ack should ignore caller cancellation after execution, got %+v", out)
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

type ctxAwareCompleteOutbox struct {
	inner port.SideEffectOutbox
}

func (o ctxAwareCompleteOutbox) Enqueue(ctx context.Context, job port.SideEffectJob) error {
	return o.inner.Enqueue(ctx, job)
}

func (o ctxAwareCompleteOutbox) Claim(ctx context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	return o.inner.Claim(ctx, opts)
}

func (o ctxAwareCompleteOutbox) Complete(ctx context.Context, jobID, leaseToken string, result port.SideEffectResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return o.inner.Complete(ctx, jobID, leaseToken, result)
}

func (o ctxAwareCompleteOutbox) Fail(ctx context.Context, jobID, leaseToken string, failure port.SideEffectFailure) error {
	return o.inner.Fail(ctx, jobID, leaseToken, failure)
}

func (o ctxAwareCompleteOutbox) Cancel(ctx context.Context, requestID string) error {
	return o.inner.Cancel(ctx, requestID)
}

func (o ctxAwareCompleteOutbox) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	return o.inner.CancelScope(ctx, scope)
}

func (o ctxAwareCompleteOutbox) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	return o.inner.PurgeScope(ctx, scope)
}

func (o ctxAwareCompleteOutbox) Stats(ctx context.Context, scope domain.Scope, now time.Time) (port.SideEffectStats, error) {
	return o.inner.Stats(ctx, scope, now)
}

type cancelingProjection struct {
	cancel context.CancelFunc
}

func (p cancelingProjection) Name() string                  { return "canceling" }
func (p cancelingProjection) Consistency() port.Consistency { return port.Required }
func (p cancelingProjection) Project(context.Context, []domain.TemporalFact) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}
func (p cancelingProjection) Forget(context.Context, domain.Scope, []string) error { return nil }
func (p cancelingProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (p cancelingProjection) ClearScope(context.Context, domain.Scope) error { return nil }

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
