package sideeffect_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/sideeffect"
)

type recordingExec struct {
	mu    sync.Mutex
	calls []port.SideEffectJobKind
	fail  error
}

func (e *recordingExec) Run(_ context.Context, job port.SideEffectJob) error {
	e.mu.Lock()
	e.calls = append(e.calls, job.Kind)
	e.mu.Unlock()
	if e.fail != nil {
		return e.fail
	}
	return nil
}

func TestQueue_ClaimCompletePreservesEnqueueOrder(t *testing.T) {
	q := sideeffect.New()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	ctx := context.Background()
	_ = q.Enqueue(ctx, port.SideEffectJob{RequestID: "b1", Scope: scope, Kind: port.SideEffectProjectRequired, Facts: []domain.TemporalFact{{ID: "f1"}}})
	_ = q.Enqueue(ctx, port.SideEffectJob{RequestID: "b1", Scope: scope, Kind: port.SideEffectEmbeddingBackfill, Facts: []domain.TemporalFact{{ID: "f1"}}})
	jobs, err := q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 2, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	if jobs[0].Kind != port.SideEffectProjectRequired || jobs[1].Kind != port.SideEffectEmbeddingBackfill {
		t.Fatalf("job order = %v, %v", jobs[0].Kind, jobs[1].Kind)
	}
	for _, job := range jobs {
		if job.LeaseToken == "" {
			t.Fatal("claimed job missing lease token")
		}
		if err := q.Complete(ctx, job.ID, job.LeaseToken, port.SideEffectResult{}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := q.Stats(ctx, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Completed != 2 || stats.Pending != 0 {
		t.Fatalf("stats = %+v, want Completed=2 Pending=0", stats)
	}
}

func TestQueue_TransientFailRetriesAfterBackoff(t *testing.T) {
	q := sideeffect.New()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	ctx := context.Background()
	_ = q.Enqueue(ctx, port.SideEffectJob{RequestID: "b1", Scope: scope, Kind: port.SideEffectProjectRequired, Facts: []domain.TemporalFact{{ID: "f1"}}})
	now := time.Now()
	jobs, err := q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: now})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%d", err, len(jobs))
	}
	retryAt := now.Add(time.Minute)
	if err := q.Fail(ctx, jobs[0].ID, jobs[0].LeaseToken, port.SideEffectFailure{RetryAt: retryAt}); err != nil {
		t.Fatal(err)
	}
	jobs, err = q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("job claimed before retryAt: %+v", jobs)
	}
	jobs, err = q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: retryAt})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != port.SideEffectProjectRequired {
		t.Fatalf("retry jobs = %+v", jobs)
	}
}

func TestQueue_PermanentFailDeadLetters(t *testing.T) {
	q := sideeffect.New()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	ctx := context.Background()
	_ = q.Enqueue(ctx, port.SideEffectJob{RequestID: "b1", Scope: scope, Kind: port.SideEffectProjectRequired})
	jobs, err := q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: time.Now()})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%d", err, len(jobs))
	}
	if err := q.Fail(ctx, jobs[0].ID, jobs[0].LeaseToken, port.SideEffectFailure{ErrClass: diagnostic.ErrClassPermanent}); err != nil {
		t.Fatal(err)
	}
	stats, err := q.Stats(ctx, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetter != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want failed dead-letter", stats)
	}
}
