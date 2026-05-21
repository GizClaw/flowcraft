package sideeffect

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestQueue_TerminalScrubsFactPayload(t *testing.T) {
	q := New()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	fact := domain.TemporalFact{
		ID:       "f1",
		Scope:    scope,
		Kind:     domain.KindNote,
		Content:  "secret content",
		Metadata: map[string]any{"secret": "value"},
	}
	if err := q.Enqueue(ctx, port.SideEffectJob{
		RequestID: "req-complete",
		Scope:     scope,
		Kind:      port.SideEffectProjectRequired,
		Facts:     []domain.TemporalFact{fact},
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: time.Now()})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%d", err, len(jobs))
	}
	if err := q.Complete(ctx, jobs[0].ID, jobs[0].LeaseToken, port.SideEffectResult{}); err != nil {
		t.Fatal(err)
	}
	stored := q.byID[jobs[0].ID].job.Facts[0]
	if stored.ID != "f1" || stored.Kind != domain.KindNote || stored.Scope.PartitionKey() != scope.PartitionKey() {
		t.Fatalf("scrubbed audit fields = %+v", stored)
	}
	if stored.Content != "" || len(stored.Metadata) != 0 {
		t.Fatalf("terminal complete retained PII: %+v", stored)
	}

	if err := q.Enqueue(ctx, port.SideEffectJob{
		RequestID: "req-fail",
		Scope:     scope,
		Kind:      port.SideEffectEmbeddingBackfill,
		Facts:     []domain.TemporalFact{fact},
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err = q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 1, Now: time.Now()})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim permanent: %v jobs=%d", err, len(jobs))
	}
	if err := q.Fail(ctx, jobs[0].ID, jobs[0].LeaseToken, port.SideEffectFailure{ErrClass: diagnostic.ErrClassPermanent}); err != nil {
		t.Fatal(err)
	}
	stored = q.byID[jobs[0].ID].job.Facts[0]
	if stored.Content != "" || len(stored.Metadata) != 0 {
		t.Fatalf("terminal failure retained PII: %+v", stored)
	}
}

func TestQueue_StatsRequiresPartition(t *testing.T) {
	q := New()
	if _, err := q.Stats(context.Background(), domain.Scope{}, time.Now()); err == nil {
		t.Fatal("Stats with empty partition must fail")
	}
}

func TestQueue_EnqueueIdempotentByRequestKind(t *testing.T) {
	q := New()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	job := port.SideEffectJob{
		RequestID: "req",
		Scope:     scope,
		Kind:      port.SideEffectProjectRequired,
		Facts:     []domain.TemporalFact{{ID: "f1"}},
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.Facts = []domain.TemporalFact{{ID: "f2"}}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.Claim(ctx, port.SideEffectClaimOptions{Scope: scope, Max: 10, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Facts[0].ID != "f1" {
		t.Fatalf("idempotent enqueue jobs = %+v", jobs)
	}
}
