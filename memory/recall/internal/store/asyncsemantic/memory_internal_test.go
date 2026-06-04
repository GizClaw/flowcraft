package asyncsemantic

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestQueue_CompleteScrubsEnqueuePII(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{
		RequestID: "req-1", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{ObservationID: "obs-1", SpanID: "span-1", Text: "secret"}},
	})
	jobs, _ := claimBatch(ctx, q, "w", time.Now(), 1)
	_ = q.Complete(ctx, jobs[0].RequestID, jobs[0].LeaseToken, port.AsyncSemanticResult{})
	e := q.byRequest["req-1"]
	if len(e.job.SourceEvidenceSpans) != 0 {
		t.Fatalf("completed job must scrub SourceEvidenceSpans, got %+v", e.job.SourceEvidenceSpans)
	}
}

func TestQueue_PermanentFailScrubsEnqueuePII(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{
		RequestID: "req-1", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		RecentMessages: []domain.Message{{Role: "user", Text: "pii"}},
	})
	jobs, _ := claimBatch(ctx, q, "w", time.Now(), 1)
	_ = q.Fail(ctx, jobs[0].RequestID, jobs[0].LeaseToken, port.AsyncSemanticFailure{
		ErrClass: diagnostic.ErrClassPermanent,
		Err:      "bad",
	})
	e := q.byRequest["req-1"]
	if len(e.job.RecentMessages) != 0 {
		t.Fatalf("dead-letter job must scrub RecentMessages, got %+v", e.job.RecentMessages)
	}
}

func TestQueue_DuplicateTransientFailDoesNotDuplicatePending(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, makeJob("req-1", "u1"))
	jobs, _ := claimBatch(ctx, q, "w", time.Now(), 1)
	fail := port.AsyncSemanticFailure{ErrClass: diagnostic.ErrClassTransient, Err: "retry"}
	_ = q.Fail(ctx, jobs[0].RequestID, jobs[0].LeaseToken, fail)
	_ = q.Fail(ctx, jobs[0].RequestID, jobs[0].LeaseToken, fail)
	claimed, _ := claimBatch(ctx, q, "w2", time.Now().Add(defaultTransientRetryBackoff+time.Second), 5)
	var matches int
	for _, j := range claimed {
		if j.RequestID == "req-1" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("duplicate Fail must not enqueue duplicate pending rows, got %d", matches)
	}
}

func TestQueue_ClaimReturnsDefensiveCopy(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{
		RequestID: "req-1", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		EpisodeFactIDs: []string{"ep-1"},
	})
	jobs, _ := claimBatch(ctx, q, "w", time.Now(), 1)
	jobs[0].EpisodeFactIDs[0] = "mutated"
	e := q.byRequest["req-1"]
	if e.job.EpisodeFactIDs[0] != "ep-1" {
		t.Fatalf("Claim must not alias queue slices, got %+v", e.job.EpisodeFactIDs)
	}
}
