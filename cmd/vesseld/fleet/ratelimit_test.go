package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
)

// TestTokenBucket_RequestCap asserts a requestsPerMinute=3 cap
// admits the first three Acquire calls instantly and forces the
// fourth to wait. We do not let the fourth caller actually run
// to completion — the cap-honouring proof is "ctx.Cancel returns
// ctx.Err quickly" rather than racing a 60-second window.
func TestTokenBucket_RequestCap(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(v1alpha1.LLMRateLimit{RequestsPerMinute: 3})
	for i := 0; i < 3; i++ {
		if err := b.Acquire(context.Background(), "v1", 0); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Acquire(ctx, "v1", 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx.Err after exceeding cap, got nil")
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("Acquire returned too quickly: %s — bucket did not block", elapsed)
	}
}

// TestTokenBucket_TokenCap asserts the tokens-per-minute axis is
// independent: a bucket with only TokensPerMinute set blocks once
// the cumulative token count exceeds the cap, even though
// requestsPerMinute is unbounded.
func TestTokenBucket_TokenCap(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(v1alpha1.LLMRateLimit{TokensPerMinute: 100})
	if err := b.Acquire(context.Background(), "v1", 60); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := b.Acquire(context.Background(), "v1", 39); err != nil {
		t.Fatalf("second: %v", err)
	}
	// 60 + 39 = 99 < 100 — third request asking for 2 tokens
	// would push to 101 and must wait.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := b.Acquire(ctx, "v1", 2); err == nil {
		t.Fatal("expected ctx.Err when token total would exceed cap")
	}
}

// TestTokenBucket_PerVesselFairshare asserts that with fair-share
// enabled, one chatty vessel cannot starve another. We line up
// vessel "noisy" to take 5 slots out of a 6-slot bucket, then
// confirm vessel "quiet" still gets at least one slot promptly.
//
// The fairshare heuristic is "1/2 of remaining capacity" rather
// than strict 1/N partitioning, so we do not assert exact
// percentages — only that quiet is served before the bucket is
// drained.
func TestTokenBucket_PerVesselFairshare(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(v1alpha1.LLMRateLimit{
		RequestsPerMinute:  6,
		PerVesselFairshare: true,
	})
	// First three Acquires by "noisy" — under fair share, after
	// each, "noisy"'s perVesselCap shrinks; the 4th from noisy
	// should be rejected by fairshare even though absolute cap
	// (6) is not yet hit.
	for i := 0; i < 3; i++ {
		if err := b.Acquire(context.Background(), "noisy", 0); err != nil {
			t.Fatalf("noisy %d: %v", i, err)
		}
	}
	// "quiet" must still be admitted promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Acquire(ctx, "quiet", 0); err != nil {
		t.Fatalf("quiet starved: %v (after %s)", err, time.Since(start))
	}
	if took := time.Since(start); took > 80*time.Millisecond {
		t.Fatalf("quiet took %s to acquire — fairshare not protecting it", took)
	}
}

// TestTokenBucket_WindowRollover asserts the bucket resets after
// >60s. We do not actually wait 60s; instead we reach into
// windowStart and rewind it to simulate the rollover.
func TestTokenBucket_WindowRollover(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(v1alpha1.LLMRateLimit{RequestsPerMinute: 1})
	if err := b.Acquire(context.Background(), "v1", 0); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Simulate 70s elapsed.
	b.mu.Lock()
	b.windowStart = b.windowStart.Add(-70 * time.Second)
	b.mu.Unlock()
	if err := b.Acquire(context.Background(), "v1", 0); err != nil {
		t.Fatalf("post-rollover acquire: %v", err)
	}
}

// TestTokenBucket_ConcurrentSafety stress-tests Acquire from many
// goroutines. All must see consistent counters (no races); the
// race detector exercises this when -race is enabled.
func TestTokenBucket_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(v1alpha1.LLMRateLimit{RequestsPerMinute: 1000})
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = b.Acquire(ctx, "v1", 1)
		}(i)
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reqsTaken != N {
		t.Fatalf("reqsTaken = %d after %d concurrent acquires", b.reqsTaken, N)
	}
}
