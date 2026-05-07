package vessel

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// tokenBudget enforces spec.Resources.MaxTokensPerTurn (per-Run cap)
// and spec.Resources.MaxTokensPerHour (vessel-wide rolling-hour cap).
//
// Engines that call host.ReportUsage feed the budget; engines that
// don't are silently uncapped. The budget therefore expresses the
// "if you tell me how much you're spending I'll enforce a cap on it"
// contract — it is opt-in from the engine side. Operators that need
// hard guarantees on every engine should pair this with a tool-level
// quota check or a TokenBudgetProbe (see probes.go).
type tokenBudget struct {
	perTurn int64
	perHour int64

	mu          sync.Mutex
	runs        map[string]*runUsage
	hourBuckets [60]int64 // index 0 is the most recent minute
	bucketStart time.Time // wall-clock start of bucket[0]
	now         func() time.Time
}

// runUsage is the per-Run accumulator. cancel is the runCtx cancel
// func captured by Captain.Submit; the budget calls it when this
// run pushes the per-turn or per-hour total past its cap so the
// engine sees ctx.Done() at the next iteration.
type runUsage struct {
	total  int64
	cancel context.CancelFunc
	runID  string
	budget *tokenBudget
}

// budgetCtxKey is the value-key under which Captain.Submit stashes
// a *runUsage on every runCtx. The sandbox host pulls it back out
// in ReportUsage. Using a private struct (vs a string) prevents
// accidental cross-package collisions.
type budgetCtxKey struct{}

// newTokenBudget returns nil when both caps are zero — the zero
// budget is the unlimited budget, and we use a nil pointer to
// avoid lock contention in the no-cap default case.
func newTokenBudget(perTurn, perHour int64) *tokenBudget {
	if perTurn <= 0 && perHour <= 0 {
		return nil
	}
	return &tokenBudget{
		perTurn: perTurn,
		perHour: perHour,
		runs:    map[string]*runUsage{},
		now:     time.Now,
	}
}

// begin registers a Run with the budget. Returns the per-run
// accumulator pointer that callers stash in ctx under budgetCtxKey.
// Safe to call when b is nil — the returned *runUsage is also nil
// and ReportUsage will silently no-op.
func (b *tokenBudget) begin(runID string, cancel context.CancelFunc) *runUsage {
	if b == nil {
		return nil
	}
	u := &runUsage{cancel: cancel, runID: runID, budget: b}
	b.mu.Lock()
	b.runs[runID] = u
	b.mu.Unlock()
	return u
}

// end drops the Run from the registry. Safe to call multiple times
// and on a nil budget; the per-Run accumulator is GC'd once both the
// runs map entry and the ctx-stashed pointer are released.
func (b *tokenBudget) end(runID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.runs, runID)
	b.mu.Unlock()
}

// add credits delta tokens against the run's per-turn counter and
// the vessel-wide hour bucket. Returns errdefs.RateLimit when either
// cap is breached; the caller is expected to invoke u.cancel() so
// the engine sees ctx.Done at the next iteration.
//
// We bias the per-turn check slightly: a Run that crosses the cap
// on the same ReportUsage call STILL gets that call accepted (we
// add the delta before checking). Otherwise small deltas right at
// the boundary would never trip the cap. The cancel that follows
// stops further LLM calls; tokens reported retroactively after
// cancellation are a deliberate accounting convenience.
func (b *tokenBudget) add(u *runUsage, delta int64) error {
	if b == nil || u == nil || delta <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rotateLocked(b.now())
	u.total += delta
	b.hourBuckets[0] += delta
	if b.perTurn > 0 && u.total > b.perTurn {
		return errdefs.RateLimitf("vessel: run %s exceeded MaxTokensPerTurn (%d > %d)", u.runID, u.total, b.perTurn)
	}
	if b.perHour > 0 && b.hourTotalLocked() > b.perHour {
		return errdefs.RateLimitf("vessel: hourly token budget exhausted (>%d)", b.perHour)
	}
	return nil
}

// hourExhausted reports whether the rolling-hour total has reached
// perHour. Used by Captain.Submit as an admission gate so callers
// see the budget breach BEFORE a Run starts (instead of mid-flight).
// Returns false on a nil budget or when perHour is zero (unlimited).
func (b *tokenBudget) hourExhausted() bool {
	if b == nil || b.perHour <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rotateLocked(b.now())
	return b.hourTotalLocked() >= b.perHour
}

// rotateLocked advances hourBuckets so bucket[0] is the current
// minute. Buckets older than 60 minutes are zeroed; partial overlap
// is shifted right and zeroed at the head. Caller must hold b.mu.
func (b *tokenBudget) rotateLocked(now time.Time) {
	cur := now.Truncate(time.Minute)
	if b.bucketStart.IsZero() {
		b.bucketStart = cur
		return
	}
	elapsed := cur.Sub(b.bucketStart) / time.Minute
	if elapsed <= 0 {
		return
	}
	if elapsed >= 60 {
		for i := range b.hourBuckets {
			b.hourBuckets[i] = 0
		}
	} else {
		shift := int(elapsed)
		copy(b.hourBuckets[shift:], b.hourBuckets[:60-shift])
		for i := 0; i < shift; i++ {
			b.hourBuckets[i] = 0
		}
	}
	b.bucketStart = cur
}

// hourTotalLocked sums every bucket. Caller must hold b.mu.
func (b *tokenBudget) hourTotalLocked() int64 {
	var s int64
	for _, v := range b.hourBuckets {
		s += v
	}
	return s
}
