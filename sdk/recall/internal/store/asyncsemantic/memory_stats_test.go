package asyncsemantic

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestQueue_StatsCountsPendingLeasedAndCancelled(t *testing.T) {
	q := New()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	now := time.Now()

	_, _ = q.Enqueue(ctx, makeJob("j1", "u1", "ep-1"))
	_, _ = q.Enqueue(ctx, makeJob("j2", "u1", "ep-2"))

	jobs, err := q.Claim(ctx, port.AsyncSemanticClaimOptions{WorkerID: "w", Now: now, Max: 1})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("claimed = %d, want 1", len(jobs))
	}

	stats, err := q.Stats(ctx, port.AsyncSemanticStatsFilter{Scope: scope, Now: now})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 1 || stats.Leased != 1 {
		t.Fatalf("stats = %+v, want Pending=1 Leased=1", stats)
	}

	_, _ = q.CancelMatchingEpisodes(ctx, scope, []string{"ep-2"})
	stats, err = q.Stats(ctx, port.AsyncSemanticStatsFilter{Now: now})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.CancelledTotal != 1 || stats.Pending != 0 || stats.Leased != 1 {
		t.Fatalf("after cancel stats = %+v", stats)
	}

	_ = q.Fail(ctx, jobs[0].RequestID, port.AsyncSemanticFailure{
		ErrClass: diagnostic.ErrClassPermanent,
		Err:      "boom",
	})
	stats, err = q.Stats(ctx, port.AsyncSemanticStatsFilter{Now: now})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.DeadLetter != 1 || stats.Failed != 1 || stats.Leased != 0 {
		t.Fatalf("after fail stats = %+v", stats)
	}
}
