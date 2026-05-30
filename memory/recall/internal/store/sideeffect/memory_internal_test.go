package sideeffect

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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
