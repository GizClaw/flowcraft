package recalltest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// AsyncSemanticQueueFactory returns a fresh, empty AsyncSemanticQueue for one subtest.
type AsyncSemanticQueueFactory func(t testing.TB) recall.AsyncSemanticQueue

// RunAsyncSemanticQueueSuite verifies the public AsyncSemanticQueue adapter contract.
func RunAsyncSemanticQueueSuite(t *testing.T, newQueue AsyncSemanticQueueFactory) {
	t.Helper()

	t.Run("enqueue is idempotent by request id", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		job := asyncSemanticJob("req-1", "ep-1")
		r1, err := queue.Enqueue(ctx, job)
		if err != nil {
			t.Fatalf("Enqueue 1: %v", err)
		}
		job.EpisodeFactIDs = []string{"mutated"}
		r2, err := queue.Enqueue(ctx, job)
		if err != nil {
			t.Fatalf("Enqueue 2: %v", err)
		}
		if r1.RequestID != r2.RequestID || !r1.EnqueuedAt.Equal(r2.EnqueuedAt) {
			t.Fatalf("idempotent receipt changed: r1=%+v r2=%+v", r1, r2)
		}
		jobs, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(jobs) != 1 || jobs[0].RequestID != "req-1" || jobs[0].EpisodeFactIDs[0] != "ep-1" {
			t.Fatalf("idempotent enqueue jobs = %+v, want original req-1", jobs)
		}
	})

	t.Run("claim respects fifo and max within scope", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		for _, id := range []string{"req-1", "req-2", "req-3"} {
			if _, err := queue.Enqueue(ctx, asyncSemanticJob(id, "ep-"+id)); err != nil {
				t.Fatalf("Enqueue %s: %v", id, err)
			}
			time.Sleep(time.Millisecond)
		}
		first, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 2)
		if err != nil {
			t.Fatalf("Claim first: %v", err)
		}
		if asyncRequestIDs(first) != "req-1,req-2" {
			t.Fatalf("first claim = %s, want req-1,req-2", asyncRequestIDs(first))
		}
		second, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 5)
		if err != nil {
			t.Fatalf("Claim second: %v", err)
		}
		if asyncRequestIDs(second) != "req-3" {
			t.Fatalf("second claim = %s, want req-3", asyncRequestIDs(second))
		}
	})

	t.Run("claim scope filter isolates partitions", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		other := recall.Scope{RuntimeID: "rt", UserID: "u2"}
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-a", "ep-a")); err != nil {
			t.Fatal(err)
		}
		otherJob := asyncSemanticJob("req-b", "ep-b")
		otherJob.Scope = other
		if _, err := queue.Enqueue(ctx, otherJob); err != nil {
			t.Fatal(err)
		}
		aJobs, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 10)
		if err != nil {
			t.Fatalf("Claim scope A: %v", err)
		}
		if asyncRequestIDs(aJobs) != "req-a" {
			t.Fatalf("scope A jobs = %s, want req-a", asyncRequestIDs(aJobs))
		}
		bJobs, err := claimAsync(ctx, queue, other, time.Unix(10, 0), 10)
		if err != nil {
			t.Fatalf("Claim scope B: %v", err)
		}
		if asyncRequestIDs(bJobs) != "req-b" {
			t.Fatalf("scope B jobs = %s, want req-b", asyncRequestIDs(bJobs))
		}
	})

	t.Run("complete is idempotent and removes from claim path", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-1", "ep-1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 1)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim = %+v, %v; want one job", jobs, err)
		}
		if err := queue.Complete(ctx, "req-1", jobs[0].LeaseToken, recall.AsyncSemanticResult{SemanticFactIDs: []string{"sf-1"}}); err != nil {
			t.Fatalf("Complete 1: %v", err)
		}
		if err := queue.Complete(ctx, "req-1", jobs[0].LeaseToken, recall.AsyncSemanticResult{}); err != nil {
			t.Fatalf("Complete 2 must be idempotent: %v", err)
		}
		if err := queue.Complete(ctx, "missing", "", recall.AsyncSemanticResult{}); err != nil {
			t.Fatalf("Complete missing must be no-op: %v", err)
		}
		stats, err := queue.Stats(ctx, recall.AsyncSemanticStatsFilter{Scope: conformanceScope(), Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Completed != 1 || stats.Pending != 0 || stats.Leased != 0 {
			t.Fatalf("stats after complete = %+v, want Completed=1 Pending=0 Leased=0", stats)
		}
	})

	t.Run("transient failure requeues after retry at", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		now := time.Now()
		retryAt := now.Add(time.Minute)
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-1", "ep-1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := claimAsync(ctx, queue, conformanceScope(), now, 1)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim = %+v, %v; want one job", jobs, err)
		}
		if err := queue.Fail(ctx, "req-1", jobs[0].LeaseToken, recall.AsyncSemanticFailure{
			ErrClass: recall.ErrClassTransient,
			Err:      "retry",
			RetryAt:  retryAt,
		}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		before, err := claimAsync(ctx, queue, conformanceScope(), retryAt.Add(-time.Second), 1)
		if err != nil {
			t.Fatalf("Claim before retry: %v", err)
		}
		if len(before) != 0 {
			t.Fatalf("claimed before retryAt: %+v", before)
		}
		after, err := claimAsync(ctx, queue, conformanceScope(), retryAt, 1)
		if err != nil {
			t.Fatalf("Claim after retry: %v", err)
		}
		if len(after) != 1 || after[0].RequestID != "req-1" || after[0].Attempt < 2 {
			t.Fatalf("retry claim = %+v, want req-1 with incremented attempt", after)
		}
	})

	t.Run("permanent failure dead letters", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		now := time.Unix(10, 0)
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-1", "ep-1")); err != nil {
			t.Fatal(err)
		}
		jobs, err := claimAsync(ctx, queue, conformanceScope(), now, 1)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim = %+v, %v; want one job", jobs, err)
		}
		if err := queue.Fail(ctx, "req-1", jobs[0].LeaseToken, recall.AsyncSemanticFailure{
			ErrClass: recall.ErrClassPermanent,
			Err:      "permanent",
		}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		stats, err := queue.Stats(ctx, recall.AsyncSemanticStatsFilter{Scope: conformanceScope(), Now: now})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Failed != 1 || stats.DeadLetter != 1 || stats.Leased != 0 {
			t.Fatalf("stats = %+v, want Failed=1 DeadLetter=1 Leased=0", stats)
		}
	})

	t.Run("lease expiry permits re-claim and stale token cannot complete", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		claimAt := time.Unix(10, 0)
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-1", "ep-1")); err != nil {
			t.Fatal(err)
		}
		first, err := claimAsync(ctx, queue, conformanceScope(), claimAt, 1)
		if err != nil || len(first) != 1 {
			t.Fatalf("first claim = %+v, %v; want one job", first, err)
		}
		staleToken := first[0].LeaseToken
		second, err := claimAsync(ctx, queue, conformanceScope(), claimAt.Add(24*time.Hour), 1)
		if err != nil || len(second) != 1 {
			t.Fatalf("second claim after lease expiry = %+v, %v; want one job", second, err)
		}
		if second[0].LeaseToken == staleToken {
			t.Fatal("re-claim should issue a fresh lease token")
		}
		if err := queue.Complete(ctx, "req-1", staleToken, recall.AsyncSemanticResult{}); err != nil {
			t.Fatalf("stale Complete must be no-op, got error: %v", err)
		}
		stats, err := queue.Stats(ctx, recall.AsyncSemanticStatsFilter{Scope: conformanceScope(), Now: claimAt.Add(24 * time.Hour)})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Completed != 0 || stats.Leased != 1 {
			t.Fatalf("stale Complete mutated state: %+v", stats)
		}
	})

	t.Run("cancel matching episodes and purge are scoped", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		ctx := context.Background()
		other := recall.Scope{RuntimeID: "rt", UserID: "u2"}
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-a", "ep-a")); err != nil {
			t.Fatal(err)
		}
		if _, err := queue.Enqueue(ctx, asyncSemanticJob("req-b", "ep-b")); err != nil {
			t.Fatal(err)
		}
		otherJob := asyncSemanticJob("req-other", "ep-a")
		otherJob.Scope = other
		if _, err := queue.Enqueue(ctx, otherJob); err != nil {
			t.Fatal(err)
		}
		cancelled, err := queue.CancelMatchingEpisodes(ctx, conformanceScope(), []string{"ep-a"})
		if err != nil {
			t.Fatalf("CancelMatchingEpisodes: %v", err)
		}
		if cancelled != 1 {
			t.Fatalf("CancelMatchingEpisodes cancelled %d, want 1", cancelled)
		}
		stats, err := queue.Stats(ctx, recall.AsyncSemanticStatsFilter{Scope: conformanceScope(), Now: time.Unix(10, 0)})
		if err != nil {
			t.Fatalf("Stats after cancel: %v", err)
		}
		if stats.CancelledTotal != 1 {
			t.Fatalf("CancelledTotal = %d, want 1", stats.CancelledTotal)
		}
		remaining, err := claimAsync(ctx, queue, conformanceScope(), time.Unix(10, 0), 10)
		if err != nil {
			t.Fatalf("Claim remaining: %v", err)
		}
		if asyncRequestIDs(remaining) != "req-b" {
			t.Fatalf("remaining same-scope jobs = %s, want req-b", asyncRequestIDs(remaining))
		}
		if err := queue.Complete(ctx, "req-b", remaining[0].LeaseToken, recall.AsyncSemanticResult{}); err != nil {
			t.Fatalf("Complete req-b: %v", err)
		}
		purged, err := queue.PurgeScope(ctx, conformanceScope())
		if err != nil {
			t.Fatalf("PurgeScope: %v", err)
		}
		if purged != 1 {
			t.Fatalf("PurgeScope removed %d, want completed req-b only", purged)
		}
		otherJobs, err := claimAsync(ctx, queue, other, time.Unix(10, 0), 10)
		if err != nil {
			t.Fatalf("Claim other scope: %v", err)
		}
		if asyncRequestIDs(otherJobs) != "req-other" {
			t.Fatalf("other scope jobs = %s, want req-other", asyncRequestIDs(otherJobs))
		}
	})

	t.Run("stats requires partition", func(t *testing.T) {
		queue := asyncSemanticQueueForTest(t, newQueue)
		if _, err := queue.Stats(context.Background(), recall.AsyncSemanticStatsFilter{Now: time.Unix(10, 0)}); err == nil {
			t.Fatal("Stats with empty scope partition must fail")
		}
	})
}

func asyncSemanticQueueForTest(t testing.TB, newQueue AsyncSemanticQueueFactory) recall.AsyncSemanticQueue {
	t.Helper()
	queue := newQueue(t)
	if queue == nil {
		t.Fatal("AsyncSemanticQueueFactory returned nil")
	}
	return queue
}

func asyncSemanticJob(requestID string, episodeIDs ...string) recall.AsyncSemanticJob {
	return recall.AsyncSemanticJob{
		RequestID:      requestID,
		Scope:          conformanceScope(),
		EpisodeFactIDs: episodeIDs,
		SourceEvidenceSpans: []recall.SourceEvidenceSpan{{
			ObservationID: "obs-" + requestID,
			SpanID:        "span-" + requestID,
			Text:          "secret text",
		}},
		RecentMessages: []recall.Message{{Role: "user", Text: "secret message"}},
	}
}

func claimAsync(ctx context.Context, queue recall.AsyncSemanticQueue, scope recall.Scope, now time.Time, max int) ([]recall.AsyncSemanticJob, error) {
	return queue.Claim(ctx, recall.AsyncSemanticClaimOptions{
		WorkerID: "recalltest-worker",
		Scope:    &scope,
		Now:      now,
		Max:      max,
	})
}

func asyncRequestIDs(jobs []recall.AsyncSemanticJob) string {
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.RequestID)
	}
	return stringsJoin(ids)
}

func stringsJoin(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out := values[0]
	for _, value := range values[1:] {
		out += "," + value
	}
	return out
}
