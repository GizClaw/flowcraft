package recall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestReconcilerStopCancelsInFlightTick(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	reg := NewMemoryNamespaceRegistry()
	scope := Scope{RuntimeID: "rt1", UserID: "u1"}
	ns := NamespaceFor(scope)
	if err := reg.Remember(ctx, ns); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{EntryToDoc(Entry{
		ID:       "entry-1",
		Scope:    scope,
		Content:  "Alice",
		Entities: []string{"alice"},
	})}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	proj := &blockingProjection{entered: make(chan struct{}), canceled: make(chan struct{})}
	r := newReconciler(idx, []Projection{proj}, reg, time.Millisecond, time.Now, nil)
	r.start()
	select {
	case <-proj.entered:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not enter projection")
	}

	done := make(chan struct{})
	go func() {
		r.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciler stop did not cancel in-flight projection")
	}
	select {
	case <-proj.canceled:
	default:
		t.Fatal("projection did not observe canceled context")
	}
}

func TestReconcilerPerScopeBudgetContinuesAfterSlowScope(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	reg := NewMemoryNamespaceRegistry()
	scopeA := Scope{RuntimeID: "rt1", UserID: "u1"}
	scopeB := Scope{RuntimeID: "rt1", UserID: "u2"}
	for _, scope := range []Scope{scopeA, scopeB} {
		ns := NamespaceFor(scope)
		if err := reg.Remember(ctx, ns); err != nil {
			t.Fatalf("Remember: %v", err)
		}
		if err := idx.Upsert(ctx, ns, []retrieval.Doc{EntryToDoc(Entry{
			ID:       "entry-" + scope.UserID,
			Scope:    scope,
			Content:  "fact",
			Entities: []string{"entity"},
		})}); err != nil {
			t.Fatalf("Upsert %s: %v", scope.UserID, err)
		}
	}

	proj := &slowFirstScopeProjection{slowUserID: "u1", sawSecond: make(chan struct{})}
	r := newReconciler(idx, []Projection{proj}, reg, 20*time.Millisecond, time.Now, nil)
	r.tick(context.Background())

	select {
	case <-proj.sawSecond:
	default:
		t.Fatal("reconciler did not continue to the next scope after the first scope exhausted its budget")
	}
}

func TestJobFailureOutcomeClassifiesCanceled(t *testing.T) {
	if got := jobFailureOutcome(context.Canceled); got != "canceled" {
		t.Fatalf("context.Canceled outcome = %q; want canceled", got)
	}
	if got := jobFailureOutcome(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("context.DeadlineExceeded outcome = %q; want timeout", got)
	}
	if got := jobFailureOutcome(errors.New("boom")); got != "failed" {
		t.Fatalf("generic error outcome = %q; want failed", got)
	}
}

type blockingProjection struct {
	entered  chan struct{}
	canceled chan struct{}
	once     chan struct{}
}

func (p *blockingProjection) Name() string { return "blocking" }
func (p *blockingProjection) Project(ctx context.Context, _ Scope, _ []Entry) error {
	return p.Replace(ctx, Scope{}, nil)
}
func (p *blockingProjection) Replace(ctx context.Context, _ Scope, _ []Entry) error {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	<-ctx.Done()
	select {
	case <-p.canceled:
	default:
		close(p.canceled)
	}
	return ctx.Err()
}
func (p *blockingProjection) Forget(context.Context, Scope, []string) error { return nil }

type slowFirstScopeProjection struct {
	slowUserID string
	sawSecond  chan struct{}
}

func (p *slowFirstScopeProjection) Name() string { return "slow_first_scope" }
func (p *slowFirstScopeProjection) Project(ctx context.Context, scope Scope, entries []Entry) error {
	return p.Replace(ctx, scope, entries)
}
func (p *slowFirstScopeProjection) Replace(ctx context.Context, scope Scope, _ []Entry) error {
	if scope.UserID == p.slowUserID {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-p.sawSecond:
	default:
		close(p.sawSecond)
	}
	return nil
}
func (p *slowFirstScopeProjection) Forget(context.Context, Scope, []string) error { return nil }
