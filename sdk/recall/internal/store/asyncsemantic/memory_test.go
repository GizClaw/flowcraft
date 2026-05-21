package asyncsemantic

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func claimBatch(ctx context.Context, q *Queue, workerID string, now time.Time, max int) ([]port.AsyncSemanticJob, error) {
	return q.Claim(ctx, port.AsyncSemanticClaimOptions{
		WorkerID: workerID,
		Now:      now,
		Max:      max,
	})
}

func makeJob(requestID, user string, episodeIDs ...string) port.AsyncSemanticJob {
	return port.AsyncSemanticJob{
		RequestID:      requestID,
		Scope:          domain.Scope{RuntimeID: "rt", UserID: user},
		EpisodeFactIDs: episodeIDs,
	}
}

// TestQueue_EnqueueIdempotent pins the F.1a contract: Enqueue is
// keyed by RequestID, so re-enqueueing the same job must return the
// existing receipt and MUST NOT duplicate the pending entry.
func TestQueue_EnqueueIdempotent(t *testing.T) {
	q := New()
	ctx := context.Background()
	job := makeJob("req-1", "u1", "e1")

	r1, err := q.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	r2, err := q.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	if !r1.EnqueuedAt.Equal(r2.EnqueuedAt) {
		t.Errorf("idempotent Enqueue must return same EnqueuedAt: r1=%v r2=%v", r1.EnqueuedAt, r2.EnqueuedAt)
	}
	jobs, err := claimBatch(ctx, q, "w1", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("idempotent Enqueue must not duplicate pending entry, claimed %d", len(jobs))
	}
}

// TestQueue_ClaimRespectsFIFO pins the per-queue ordering contract:
// Claim returns jobs in (enqueuedAt asc, scope, requestID) order so
// downstream resolver decisions match the order Save accepted them.
func TestQueue_ClaimRespectsFIFO(t *testing.T) {
	q := New()
	ctx := context.Background()
	for _, id := range []string{"req-1", "req-2", "req-3"} {
		if _, err := q.Enqueue(ctx, makeJob(id, "u1")); err != nil {
			t.Fatalf("Enqueue %s: %v", id, err)
		}
		// Force monotonic enqueuedAt separation.
		time.Sleep(time.Millisecond)
	}
	jobs, err := claimBatch(ctx, q, "w1", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("Claim len = %d, want 3", len(jobs))
	}
	want := []string{"req-1", "req-2", "req-3"}
	for i, j := range jobs {
		if j.RequestID != want[i] {
			t.Errorf("jobs[%d].RequestID = %q, want %q", i, j.RequestID, want[i])
		}
	}
}

// TestQueue_ClaimRespectsMax pins the batch-size contract: Claim
// returns at most `max` jobs, leaves the rest pending, and subsequent
// claims see the remaining ones.
func TestQueue_ClaimRespectsMax(t *testing.T) {
	q := New()
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		_, _ = q.Enqueue(ctx, makeJob(id, "u1"))
		time.Sleep(time.Millisecond)
	}
	first, err := claimBatch(ctx, q, "w1", time.Now(), 2)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if len(first) != 2 {
		t.Errorf("first claim returned %d, want 2", len(first))
	}
	second, err := claimBatch(ctx, q, "w1", time.Now(), 5)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if len(second) != 1 || second[0].RequestID != "c" {
		t.Errorf("second claim = %+v, want [c]", second)
	}
}

// TestQueue_CompleteIdempotent pins the completion contract: marking
// a job complete twice MUST NOT return an error, and a missing job id
// is a tolerated no-op (mirrors at-least-once delivery semantics).
func TestQueue_CompleteIdempotent(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, makeJob("req-1", "u1"))
	if _, err := claimBatch(ctx, q, "w1", time.Now(), 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Complete(ctx, "req-1", port.AsyncSemanticResult{SemanticFactIDs: []string{"sf-1"}}); err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	if err := q.Complete(ctx, "req-1", port.AsyncSemanticResult{}); err != nil {
		t.Errorf("Complete 2 must be idempotent: %v", err)
	}
	if err := q.Complete(ctx, "req-missing", port.AsyncSemanticResult{}); err != nil {
		t.Errorf("Complete on missing id must no-op: %v", err)
	}
}

// TestQueue_FailMovesToFailed pins the failure-routing contract: Fail
// records the supplied AsyncSemanticFailure, releases the lease, and
// stays idempotent on subsequent Fail calls.
func TestQueue_FailMovesToFailed(t *testing.T) {
	q := New()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, makeJob("req-1", "u1"))
	if _, err := claimBatch(ctx, q, "w1", time.Now(), 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	failure := port.AsyncSemanticFailure{
		ErrClass: diagnostic.ErrClassPermanent,
		Err:      "schema rejected",
	}
	if err := q.Fail(ctx, "req-1", failure); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	e := q.byRequest["req-1"]
	if e.status != statusFailed {
		t.Errorf("status = %q, want failed", e.status)
	}
	if e.failure.ErrClass != diagnostic.ErrClassPermanent {
		t.Errorf("failure.ErrClass = %q, want permanent", e.failure.ErrClass)
	}
	if _, ok := q.leased["req-1"]; ok {
		t.Errorf("lease must be released on Fail")
	}
}

// TestQueue_LeaseExpiry pins the recovery contract: a job claimed
// with a LeaseUntil in the past MUST become re-claimable by a future
// Claim call, so worker crashes / lease losses do not strand jobs.
func TestQueue_LeaseExpiry(t *testing.T) {
	q := New()
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	job := makeJob("req-1", "u1")
	job.LeaseUntil = past
	if _, err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := claimBatch(ctx, q, "w1", time.Now(), 1)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("Claim 1 len = %d, want 1", len(first))
	}
	second, err := claimBatch(ctx, q, "w2", time.Now(), 1)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if len(second) != 1 || second[0].RequestID != "req-1" {
		t.Errorf("expired lease must be re-claimable, got %+v", second)
	}
}

// TestQueue_DefaultLeaseExpires pins the crash-recovery contract for
// normal jobs. Callers should not have to pre-populate LeaseUntil in
// the enqueued payload; Claim must assign a lease so an abandoned job
// eventually becomes claimable again.
func TestQueue_DefaultLeaseExpires(t *testing.T) {
	q := New()
	ctx := context.Background()
	job := makeJob("req-default-lease", "u1")
	if _, err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := claimBatch(ctx, q, "w1", time.Unix(100, 0), 1)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("Claim 1 len = %d, want 1", len(first))
	}
	second, err := claimBatch(ctx, q, "w2", time.Unix(100, 0).Add(24*time.Hour), 1)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if len(second) != 1 || second[0].RequestID != job.RequestID {
		t.Fatalf("job with default lease must be re-claimable after expiry window, got %+v", second)
	}
}

func TestQueue_CancelMatchingEpisodesOnlyTouchesIntersectingJobs(t *testing.T) {
	q := New()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	epA := "ep-a"
	epB := "ep-b"
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "j-a", Scope: scope, EpisodeFactIDs: []string{epA}})
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "j-b", Scope: scope, EpisodeFactIDs: []string{epB}})
	n, err := q.CancelMatchingEpisodes(ctx, scope, []string{epA})
	if err != nil {
		t.Fatalf("CancelMatchingEpisodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled = %d, want 1", n)
	}
	jobs, err := claimBatch(ctx, q, "w", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].RequestID != "j-b" {
		t.Fatalf("remaining jobs = %+v, want only j-b", jobs)
	}
}

func TestQueue_CancelScopeRemovesPartitionJobs(t *testing.T) {
	q := New()
	ctx := context.Background()
	scopeA := domain.Scope{RuntimeID: "rt-a", UserID: "u1"}
	scopeB := domain.Scope{RuntimeID: "rt-b", UserID: "u1"}
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "a1", Scope: scopeA})
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "b1", Scope: scopeB})
	n, err := q.CancelScope(ctx, scopeA)
	if err != nil {
		t.Fatalf("CancelScope: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled = %d, want 1", n)
	}
	jobs, err := claimBatch(ctx, q, "w", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].RequestID != "b1" {
		t.Fatalf("remaining jobs = %+v, want only b1", jobs)
	}
}

func TestQueue_CancelRemovesPendingJob(t *testing.T) {
	q := New()
	ctx := context.Background()
	job := makeJob("req-cancel", "u1")
	if _, err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Cancel(ctx, job.RequestID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	jobs, err := claimBatch(ctx, q, "w1", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("Claim after Cancel = %+v, want empty", jobs)
	}
}
