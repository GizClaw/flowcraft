package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
)

// tokenBucket is a tiny rate limiter sized in requests-per-minute.
// Internally we keep a per-minute window and a per-vessel tally
// (when PerVesselFairshare is true) so a single chatty vessel
// cannot starve siblings.
//
// We avoid pulling in golang.org/x/time/rate to keep the daemon
// dependency footprint small; the bucket here is good enough for
// the "soft global ceiling" use case the daemon ships with. When
// the rate-limit story gets richer (per-token accounting,
// distributed buckets) v0.2.0+ can swap in a richer impl behind
// the same RateLimiter interface.
type tokenBucket struct {
	mu                sync.Mutex
	requestsPerMinute int
	tokensPerMinute   int
	perVesselFair     bool

	windowStart time.Time
	reqsTaken   int
	tokensTaken int

	perVesselReqs   map[string]int
	perVesselTokens map[string]int
}

// newTokenBucket returns a bucket sized for the daemon-config
// rate limit. requestsPerMinute / tokensPerMinute = 0 disables
// that dimension (no cap on that axis).
func newTokenBucket(rl v1alpha1.LLMRateLimit) *tokenBucket {
	return &tokenBucket{
		requestsPerMinute: rl.RequestsPerMinute,
		tokensPerMinute:   rl.TokensPerMinute,
		perVesselFair:     rl.PerVesselFairshare,
		windowStart:       time.Now(),
		perVesselReqs:     map[string]int{},
		perVesselTokens:   map[string]int{},
	}
}

// Acquire blocks until 1 request and `tokens` tokens are
// available, or until ctx is cancelled. Returns ctx.Err on
// cancellation.
//
// vesselID is used for per-vessel fair-share accounting; pass ""
// when fair-share is disabled.
func (b *tokenBucket) Acquire(ctx context.Context, vesselID string, tokens int) error {
	for {
		b.mu.Lock()
		b.rolloverLocked()

		// Per-vessel fair share: each vessel may take at most
		// 1/N of the bucket where N is the number of unique
		// vessels seen this window. Implementation is the
		// "1/2 of remaining" heuristic — exact fairness across N
		// is complex; this approximation prevents one vessel
		// from monopolising the bucket while staying simple.
		fairshare := func(taken, perVesselTaken, cap int) bool {
			if !b.perVesselFair {
				return true
			}
			remaining := cap - taken
			perVesselCap := perVesselTaken + (remaining+1)/2
			if perVesselCap < taken {
				perVesselCap = taken
			}
			return perVesselTaken < perVesselCap
		}

		ok := true
		if b.requestsPerMinute > 0 {
			if b.reqsTaken >= b.requestsPerMinute {
				ok = false
			} else if !fairshare(b.reqsTaken, b.perVesselReqs[vesselID], b.requestsPerMinute) {
				ok = false
			}
		}
		if ok && b.tokensPerMinute > 0 && tokens > 0 {
			if b.tokensTaken+tokens > b.tokensPerMinute {
				ok = false
			} else if !fairshare(b.tokensTaken, b.perVesselTokens[vesselID], b.tokensPerMinute) {
				ok = false
			}
		}
		if ok {
			b.reqsTaken++
			b.tokensTaken += tokens
			b.perVesselReqs[vesselID]++
			b.perVesselTokens[vesselID] += tokens
			b.mu.Unlock()
			return nil
		}
		// Sleep until either the next window rollover or ctx.
		wait := time.Until(b.windowStart.Add(time.Minute))
		b.mu.Unlock()
		if wait <= 0 {
			wait = 50 * time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// rolloverLocked resets the per-window counters when more than
// 60s has passed since the window started. Caller must hold mu.
func (b *tokenBucket) rolloverLocked() {
	if time.Since(b.windowStart) < time.Minute {
		return
	}
	b.windowStart = time.Now()
	b.reqsTaken = 0
	b.tokensTaken = 0
	b.perVesselReqs = map[string]int{}
	b.perVesselTokens = map[string]int{}
}

// buildLimiters creates one tokenBucket per LLMRateLimit entry,
// keyed by LLMProfile name. Engine factories pull the right one
// out via Limiter(profile) and call Acquire before each LLM
// request.
func buildLimiters(limits []v1alpha1.LLMRateLimit) map[string]*tokenBucket {
	out := make(map[string]*tokenBucket, len(limits))
	for _, rl := range limits {
		out[rl.LLMProfile] = newTokenBucket(rl)
	}
	return out
}

// Limiter returns the rate limiter for a profile, or nil when no
// limit is configured. nil callers should treat as "no limit".
func (f *Fleet) Limiter(profile string) *tokenBucket {
	if f.limiters == nil {
		return nil
	}
	return f.limiters[profile]
}
