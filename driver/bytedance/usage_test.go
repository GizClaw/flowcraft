package bytedance

import "testing"

// TestRawUsageCanonicalDerivesUncachedTokens pins the input split on the
// Ark wire shape: prompt_tokens is the inclusive total (cached tokens are a
// subset), and the uncached remainder is derived so consumers can compute a
// cache-hit rate as cache_read / input_tokens on every provider.
func TestRawUsageCanonicalDerivesUncachedTokens(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:  49224,
		outputTokens: 100,
		totalTokens:  49324,
		cachedTokens: 48000,
	})
	if usage.InputTokens != 49224 {
		t.Fatalf("input tokens = %d, want the wire prompt_tokens", usage.InputTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 1224 {
		t.Fatalf("uncached tokens = %v, want 1224", usage.Input.UncachedTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestRawUsageCanonicalNoUsageReported pins the absence case: nothing was
// reported, so nothing may be invented.
func TestRawUsageCanonicalNoUsageReported(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{})
	if usage.InputTokens != 0 || usage.Input.UncachedTokens != nil {
		t.Fatalf("usage = %+v, want nothing reported", usage)
	}
}

// TestRawUsageCanonicalClampsContradictoryCache pins the malformed-wire
// path: a cached count larger than the prompt is clamped so the input
// sub-counters still partition the prompt total instead of exceeding it.
func TestRawUsageCanonicalClampsContradictoryCache(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:  10,
		outputTokens: 1,
		totalTokens:  11,
		cachedTokens: 99,
	})
	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 10 {
		t.Fatalf("cache read tokens = %v, want a read clamped to the prompt",
			usage.Input.CacheReadTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 0 {
		t.Fatalf("uncached tokens = %v, want zero", usage.Input.UncachedTokens)
	}
	if got := *usage.Input.UncachedTokens + *usage.Input.CacheReadTokens; got != usage.InputTokens {
		t.Fatalf("uncached + read = %d, want the input total %d", got, usage.InputTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
