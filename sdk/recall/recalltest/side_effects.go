package recalltest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

// SideEffectOutboxFactory returns a fresh, empty SideEffectOutbox for one subtest.
type SideEffectOutboxFactory func(t testing.TB) recall.SideEffectOutbox

// RunSideEffectOutboxSuite verifies the public SideEffectOutbox adapter contract.
func RunSideEffectOutboxSuite(t *testing.T, newOutbox SideEffectOutboxFactory) {
	t.Helper()

	t.Run("enqueue is idempotent by request and kind", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		ctx := context.Background()
		job := sideEffectJob("req-1", recall.SideEffectProjectRequired, "f1")
		if err := outbox.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue 1: %v", err)
		}
		job.Facts = []recall.TemporalFact{{ID: "mutated"}}
		if err := outbox.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue 2: %v", err)
		}
		jobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 10, Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("claimed %d jobs, want 1", len(jobs))
		}
		if len(jobs[0].Facts) != 1 || jobs[0].Facts[0].ID != "f1" {
			t.Fatalf("idempotent enqueue must keep original payload, got %+v", jobs[0].Facts)
		}
	})

	t.Run("claim complete preserves enqueue order", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		ctx := context.Background()
		if err := outbox.Enqueue(ctx, sideEffectJob("req-1", recall.SideEffectProjectRequired, "f1")); err != nil {
			t.Fatal(err)
		}
		if err := outbox.Enqueue(ctx, sideEffectJob("req-1", recall.SideEffectEmbeddingBackfill, "f1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 2, Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(jobs) != 2 {
			t.Fatalf("claimed %d jobs, want 2", len(jobs))
		}
		if jobs[0].Kind != recall.SideEffectProjectRequired || jobs[1].Kind != recall.SideEffectEmbeddingBackfill {
			t.Fatalf("job order = %s,%s", jobs[0].Kind, jobs[1].Kind)
		}
		for _, job := range jobs {
			if job.ID == "" || job.LeaseToken == "" {
				t.Fatalf("claimed job missing ID/LeaseToken: %+v", job)
			}
			if err := outbox.Complete(ctx, job.ID, job.LeaseToken, recall.SideEffectResult{CompletedAt: time.Unix(11, 0)}); err != nil {
				t.Fatalf("Complete %s: %v", job.ID, err)
			}
		}
		stats, err := outbox.Stats(ctx, conformanceScope(), time.Unix(12, 0))
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Completed != 2 || stats.Pending != 0 || stats.Leased != 0 {
			t.Fatalf("stats after complete = %+v, want Completed=2 Pending=0 Leased=0", stats)
		}
	})

	t.Run("transient failure retries after retry at", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		ctx := context.Background()
		now := time.Unix(10, 0)
		retryAt := now.Add(time.Minute)
		if err := outbox.Enqueue(ctx, sideEffectJob("req-1", recall.SideEffectProjectRequired, "f1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 1, Now: now})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim = %+v, %v; want one job", jobs, err)
		}
		if err := outbox.Fail(ctx, jobs[0].ID, jobs[0].LeaseToken, recall.SideEffectFailure{
			ErrClass: recall.ErrClassTransient,
			Err:      "retry",
			RetryAt:  retryAt,
		}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		before, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 1, Now: retryAt.Add(-time.Second)})
		if err != nil {
			t.Fatalf("Claim before retry: %v", err)
		}
		if len(before) != 0 {
			t.Fatalf("claimed before retryAt: %+v", before)
		}
		after, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 1, Now: retryAt})
		if err != nil {
			t.Fatalf("Claim after retry: %v", err)
		}
		if len(after) != 1 || after[0].Attempt < 2 {
			t.Fatalf("retry claim = %+v, want one retried job", after)
		}
	})

	t.Run("permanent failure dead letters", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		ctx := context.Background()
		now := time.Unix(10, 0)
		if err := outbox.Enqueue(ctx, sideEffectJob("req-1", recall.SideEffectProjectRequired, "f1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 1, Now: now})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim = %+v, %v; want one job", jobs, err)
		}
		if err := outbox.Fail(ctx, jobs[0].ID, jobs[0].LeaseToken, recall.SideEffectFailure{
			ErrClass: recall.ErrClassPermanent,
			Err:      "permanent",
		}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		stats, err := outbox.Stats(ctx, conformanceScope(), now)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Failed != 1 || stats.DeadLetter != 1 {
			t.Fatalf("stats = %+v, want Failed=1 DeadLetter=1", stats)
		}
	})

	t.Run("cancel and purge are scoped", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		ctx := context.Background()
		other := recall.Scope{RuntimeID: "rt", UserID: "u2"}
		if err := outbox.Enqueue(ctx, sideEffectJob("req-a", recall.SideEffectProjectRequired, "fa")); err != nil {
			t.Fatal(err)
		}
		otherJob := sideEffectJob("req-b", recall.SideEffectProjectRequired, "fb")
		otherJob.Scope = other
		if err := outbox.Enqueue(ctx, otherJob); err != nil {
			t.Fatal(err)
		}
		if err := outbox.Cancel(ctx, "req-a"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		jobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: conformanceScope(), Max: 10, Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Claim cancelled scope: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("cancelled job still claimable: %+v", jobs)
		}
		stats, err := outbox.Stats(ctx, conformanceScope(), time.Unix(10, 0))
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.CancelledTotal != 1 {
			t.Fatalf("CancelledTotal = %d, want 1", stats.CancelledTotal)
		}
		purged, err := outbox.PurgeScope(ctx, conformanceScope())
		if err != nil {
			t.Fatalf("PurgeScope: %v", err)
		}
		if purged != 0 {
			t.Fatalf("PurgeScope after cancel removed %d, want 0", purged)
		}
		otherJobs, err := outbox.Claim(ctx, recall.SideEffectClaimOptions{Scope: other, Max: 10, Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Claim other scope: %v", err)
		}
		if len(otherJobs) != 1 || otherJobs[0].RequestID != "req-b" {
			t.Fatalf("other scope jobs = %+v, want req-b", otherJobs)
		}
	})

	t.Run("stats requires partition", func(t *testing.T) {
		outbox := sideEffectOutboxForTest(t, newOutbox)
		if _, err := outbox.Stats(context.Background(), recall.Scope{}, time.Unix(10, 0)); err == nil {
			t.Fatal("Stats with empty scope partition must fail")
		}
	})
}

func sideEffectOutboxForTest(t testing.TB, newOutbox SideEffectOutboxFactory) recall.SideEffectOutbox {
	t.Helper()
	outbox := newOutbox(t)
	if outbox == nil {
		t.Fatal("SideEffectOutboxFactory returned nil")
	}
	return outbox
}

func sideEffectJob(requestID string, kind recall.SideEffectJobKind, factID string) recall.SideEffectJob {
	return recall.SideEffectJob{
		RequestID: requestID,
		Scope:     conformanceScope(),
		Kind:      kind,
		Facts: []recall.TemporalFact{{
			ID:      factID,
			Scope:   conformanceScope(),
			Kind:    recall.FactNote,
			Content: "secret-" + factID,
		}},
	}
}
