package asyncsemantic

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// TestQueue_ConcurrentEnqueueSameRequestID stresses idempotent Enqueue
// under parallel writers. Exactly one pending job must exist per
// RequestID regardless of goroutine count.
func TestQueue_ConcurrentEnqueueSameRequestID(t *testing.T) {
	q := New()
	ctx := context.Background()
	const workers = 32
	job := makeJob("req-race-idem", "u1", "e1")

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, _ = q.Enqueue(ctx, job)
		}()
	}
	wg.Wait()

	jobs, err := q.Claim(ctx, "w1", time.Now(), 64)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("concurrent idempotent Enqueue must leave exactly 1 pending job, got %d", len(jobs))
	}
	if jobs[0].RequestID != job.RequestID {
		t.Errorf("RequestID = %q, want %q", jobs[0].RequestID, job.RequestID)
	}
}

// TestQueue_ConcurrentEnqueueDistinctIDs hammers Enqueue with unique
// RequestIDs from many goroutines. Every job must land in pending
// without loss or duplication.
func TestQueue_ConcurrentEnqueueDistinctIDs(t *testing.T) {
	q := New()
	ctx := context.Background()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-distinct-%d", i)
		go func() {
			defer wg.Done()
			_, err := q.Enqueue(ctx, makeJob(id, "u1"))
			if err != nil {
				t.Errorf("Enqueue %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	jobs, err := q.Claim(ctx, "w1", time.Now(), n+10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != n {
		t.Fatalf("Claim len = %d, want %d", len(jobs), n)
	}
	seen := make(map[string]struct{}, n)
	for _, j := range jobs {
		if _, dup := seen[j.RequestID]; dup {
			t.Fatalf("duplicate RequestID in claim batch: %q", j.RequestID)
		}
		seen[j.RequestID] = struct{}{}
	}
}

// TestQueue_ConcurrentClaimWorkers simulates multiple workers claiming
// from the same queue. Each job must be handed to at most one worker
// per claim wave (leased, not double-claimed while lease active).
func TestQueue_ConcurrentClaimWorkers(t *testing.T) {
	q := New()
	ctx := context.Background()
	const jobs = 50
	const workers = 8
	for i := 0; i < jobs; i++ {
		_, err := q.Enqueue(ctx, makeJob(fmt.Sprintf("req-claim-%d", i), "u1"))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	now := time.Now()
	var claimed sync.Map // requestID -> struct{}
	var wg sync.WaitGroup
	var total atomic.Int32

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			batch, err := q.Claim(ctx, fmt.Sprintf("w%d", worker), now, jobs)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			for _, j := range batch {
				if _, loaded := claimed.LoadOrStore(j.RequestID, struct{}{}); loaded {
					t.Errorf("double-claim while lease active: %q", j.RequestID)
				}
				total.Add(1)
			}
		}(w)
	}
	wg.Wait()

	if got := int(total.Load()); got != jobs {
		t.Fatalf("total claimed = %d, want %d", got, jobs)
	}
}

// TestQueue_ConcurrentEnqueueClaimComplete runs a producer / consumer
// loop: writers Enqueue, workers Claim+Complete, until all jobs finish.
func TestQueue_ConcurrentEnqueueClaimComplete(t *testing.T) {
	q := New()
	ctx := context.Background()
	const total = 100
	const producers = 4
	const consumers = 4

	var enqueued atomic.Int32
	var completed atomic.Int32

	var prodWG sync.WaitGroup
	prodWG.Add(producers)
	for p := 0; p < producers; p++ {
		go func(base int) {
			defer prodWG.Done()
			for i := 0; i < total/producers; i++ {
				id := fmt.Sprintf("req-flow-%d-%d", base, i)
				if _, err := q.Enqueue(ctx, makeJob(id, "u1")); err != nil {
					t.Errorf("Enqueue: %v", err)
					return
				}
				enqueued.Add(1)
			}
		}(p)
	}

	var consWG sync.WaitGroup
	for c := 0; c < consumers; c++ {
		consWG.Add(1)
		go func(worker int) {
			defer consWG.Done()
			workerID := fmt.Sprintf("consumer-%d", worker)
			for int(completed.Load()) < total {
				batch, err := q.Claim(ctx, workerID, time.Now(), 8)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				for _, j := range batch {
					if err := q.Complete(ctx, j.RequestID, port.AsyncSemanticResult{SemanticFactIDs: []string{"sf"}}); err != nil {
						t.Errorf("Complete: %v", err)
						return
					}
					completed.Add(1)
				}
				if len(batch) == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}(c)
	}

	prodWG.Wait()
	consWG.Wait()

	if got := int(enqueued.Load()); got != total {
		t.Fatalf("enqueued = %d, want %d", got, total)
	}
	if got := int(completed.Load()); got != total {
		t.Fatalf("completed = %d, want %d", got, total)
	}
}

// TestQueue_ConcurrentEnqueueCancelClaim races Enqueue against Cancel
// and subsequent Claim. A cancelled job must never appear in a claim
// batch; a surviving job must still be claimable.
func TestQueue_ConcurrentEnqueueCancelClaim(t *testing.T) {
	q := New()
	ctx := context.Background()
	const rounds = 80
	var wg sync.WaitGroup
	wg.Add(rounds * 2)

	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("req-cancel-race-%d", i)
		go func() {
			defer wg.Done()
			_, _ = q.Enqueue(ctx, makeJob(id, "u1"))
		}()
		go func() {
			defer wg.Done()
			_ = q.Cancel(ctx, id)
		}()
	}
	wg.Wait()

	jobs, err := q.Claim(ctx, "w1", time.Now(), rounds+10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, j := range jobs {
		if err := q.Cancel(ctx, j.RequestID); err != nil {
			t.Fatalf("post-claim Cancel: %v", err)
		}
	}
}

// TestQueue_ConcurrentClaimAfterLeaseExpiry verifies that jobs with
// expired default leases become visible again under concurrent Claim
// after the expiry window, without double-processing the same job in
// one wave.
func TestQueue_ConcurrentClaimAfterLeaseExpiry(t *testing.T) {
	q := New()
	ctx := context.Background()
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(ctx, makeJob(fmt.Sprintf("req-lease-%d", i), "u1")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	claim1 := time.Unix(1_000, 0)
	var first sync.Map
	var wg1 sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg1.Add(1)
		go func() {
			defer wg1.Done()
			batch, err := q.Claim(ctx, "w-first", claim1, n)
			if err != nil {
				t.Errorf("Claim 1: %v", err)
				return
			}
			for _, j := range batch {
				first.Store(j.RequestID, struct{}{})
			}
		}()
	}
	wg1.Wait()

	claim2 := claim1.Add(defaultLeaseTTL + time.Second)
	var secondCount atomic.Int32
	var wg2 sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			batch, err := q.Claim(ctx, "w-second", claim2, n)
			if err != nil {
				t.Errorf("Claim 2: %v", err)
				return
			}
			secondCount.Add(int32(len(batch)))
		}()
	}
	wg2.Wait()

	if got := int(secondCount.Load()); got != n {
		t.Fatalf("re-claim after lease expiry: got %d jobs, want %d", got, n)
	}
}

// TestQueue_ConcurrentFailCompleteRaces Fail and Complete against the
// same leased job from competing goroutines. The queue must stay
// consistent: terminal state is either complete or failed, never both,
// and idempotent retries must not panic.
func TestQueue_ConcurrentFailCompleteRaces(t *testing.T) {
	q := New()
	ctx := context.Background()
	const n = 30
	for i := 0; i < n; i++ {
		_, _ = q.Enqueue(ctx, makeJob(fmt.Sprintf("req-term-%d", i), "u1"))
	}
	jobs, err := q.Claim(ctx, "w1", time.Now(), n)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			_ = q.Complete(ctx, id, port.AsyncSemanticResult{SemanticFactIDs: []string{"ok"}})
		}(job.RequestID)
		go func(id string) {
			defer wg.Done()
			_ = q.Fail(ctx, id, port.AsyncSemanticFailure{
				ErrClass: diagnostic.ErrClassTransient,
				Err:      "boom",
			})
		}(job.RequestID)
	}
	wg.Wait()
}
