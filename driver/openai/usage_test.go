package openai

import "testing"

// TestRawUsageCanonicalDerivesUncachedTokens pins the input split on the
// OpenAI wire shape. prompt_tokens is already the inclusive total (cached
// tokens are a subset of it), so nothing is normalized; the uncached
// remainder is derived so the input sub-counters partition the total the
// same way they do for providers with exclusive counters.
func TestRawUsageCanonicalDerivesUncachedTokens(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:  49224,
		outputTokens: 100,
		totalTokens:  49324,
		cachedTokens: 48000,
	})
	if usage.InputTokens != 49224 {
		t.Fatalf("input tokens = %d, want the wire prompt_tokens 49224", usage.InputTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 1224 {
		t.Fatalf("uncached tokens = %v, want 1224", usage.Input.UncachedTokens)
	}
	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 48000 {
		t.Fatalf("cache read tokens = %v, want 48000", usage.Input.CacheReadTokens)
	}
	if got := *usage.Input.UncachedTokens + *usage.Input.CacheReadTokens; got != usage.InputTokens {
		t.Fatalf("uncached + read = %d, want the input total %d", got, usage.InputTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestRawUsageCanonicalFullyCachedReportsZero pins the fully cached case:
// the derived remainder of zero is a measurement, and the cache-write
// bucket is subtracted from it rather than ignored.
func TestRawUsageCanonicalFullyCachedReportsZero(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:      100,
		outputTokens:     5,
		totalTokens:      105,
		cachedTokens:     80,
		cacheWriteTokens: 20,
	})
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 0 {
		t.Fatalf("uncached tokens = %v, want a reported zero", usage.Input.UncachedTokens)
	}
}

// TestRawUsageCanonicalClampsContradictoryCounters pins the malformed-wire
// path: a bucket larger than the prompt, or a negative count, is clamped so
// the input sub-counters still partition the prompt total instead of
// exceeding it.
func TestRawUsageCanonicalClampsContradictoryCounters(t *testing.T) {
	odd := rawUsageCanonical(rawUsage{
		inputTokens:      10,
		outputTokens:     1,
		totalTokens:      11,
		cachedTokens:     99,
		cacheWriteTokens: 4,
	})
	if odd.Input.UncachedTokens == nil || *odd.Input.UncachedTokens != 0 {
		t.Fatalf("uncached tokens = %v, want a clamped zero", odd.Input.UncachedTokens)
	}
	if odd.Input.CacheReadTokens == nil || *odd.Input.CacheReadTokens != 10 {
		t.Fatalf("cache read tokens = %v, want a read clamped to the prompt",
			odd.Input.CacheReadTokens)
	}
	if odd.Input.CacheWriteTokens != nil {
		t.Fatalf("cache write tokens = %v, want none left for writes",
			odd.Input.CacheWriteTokens)
	}
	if got := *odd.Input.UncachedTokens + *odd.Input.CacheReadTokens; got != odd.InputTokens {
		t.Fatalf("uncached + read = %d, want the input total %d", got, odd.InputTokens)
	}
	if err := odd.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	negative := rawUsageCanonical(rawUsage{
		inputTokens:  10,
		outputTokens: 1,
		totalTokens:  11,
		cachedTokens: -3,
	})
	if negative.Input.CacheReadTokens != nil {
		t.Fatalf("cache read tokens = %v, want none reported",
			negative.Input.CacheReadTokens)
	}
	if negative.Input.UncachedTokens == nil || *negative.Input.UncachedTokens != 10 {
		t.Fatalf("uncached tokens = %v, want the full prompt 10",
			negative.Input.UncachedTokens)
	}
	if err := negative.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestRawUsageCanonicalNoUsageReported pins the absence case: a provider
// that reported no counters must not gain a derived zero bucket.
func TestRawUsageCanonicalNoUsageReported(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{})
	if usage.InputTokens != 0 || usage.Input.UncachedTokens != nil {
		t.Fatalf("usage = %+v, want nothing reported", usage)
	}
}
