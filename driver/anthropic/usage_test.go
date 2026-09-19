package anthropic

import "testing"

// TestRawUsageCanonicalNormalizesCacheCounters pins the wire-to-canonical
// mapping of Anthropic usage. Anthropic's input_tokens counts only the
// tokens that were neither read from nor written to the cache, so a
// request served mostly from cache carries a tiny input_tokens next to a
// huge cache_read_input_tokens. Core's Usage.InputTokens is the inclusive
// prompt total, so the canonical value is the sum of the three wire
// buckets: a host that spent the raw bucket as the prompt size (compaction
// thresholds, context budgets, cache-hit rates) would treat a fully cached
// long conversation as an empty one.
func TestRawUsageCanonicalNormalizesCacheCounters(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:      200,
		outputTokens:     100,
		cacheReadTokens:  48000,
		cacheWriteTokens: 1024,
		cacheWrite5m:     1024,
	})
	if usage.InputTokens != 49224 {
		t.Fatalf("input tokens = %d, want the inclusive total 49224", usage.InputTokens)
	}
	if usage.TotalTokens != 49324 {
		t.Fatalf("total tokens = %d, want input + output", usage.TotalTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 200 {
		t.Fatalf("uncached tokens = %v, want the exclusive bucket 200", usage.Input.UncachedTokens)
	}
	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 48000 {
		t.Fatalf("cache read tokens = %v, want 48000", usage.Input.CacheReadTokens)
	}
	if usage.Input.CacheWriteTokens == nil || *usage.Input.CacheWriteTokens != 1024 {
		t.Fatalf("cache write tokens = %v, want 1024", usage.Input.CacheWriteTokens)
	}
	// The partition is the invariant consumers rely on.
	if got := *usage.Input.UncachedTokens +
		*usage.Input.CacheReadTokens +
		*usage.Input.CacheWriteTokens; got != usage.InputTokens {
		t.Fatalf("uncached + read + write = %d, want the input total %d",
			got, usage.InputTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("normalized usage must validate: %v", err)
	}
}

// TestRawUsageCanonicalWithoutCache pins the ordinary path: no cache
// counters means the whole prompt is uncached, and nothing is invented.
func TestRawUsageCanonicalWithoutCache(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{inputTokens: 1000, outputTokens: 40})
	if usage.InputTokens != 1000 || usage.TotalTokens != 1040 {
		t.Fatalf("usage = %+v, want 1000 in / 1040 total", usage)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 1000 {
		t.Fatalf("uncached tokens = %v, want 1000", usage.Input.UncachedTokens)
	}
	if usage.Input.CacheReadTokens != nil || usage.Input.CacheWriteTokens != nil {
		t.Fatal("no cache counters were reported; none may be invented")
	}
}

// TestRawUsageCanonicalFullyCachedKeepsZeroUncached pins the edge case a
// pointer exists for: a prompt served entirely from cache reports zero
// uncached tokens, which is a measurement, not an absence.
func TestRawUsageCanonicalFullyCachedKeepsZeroUncached(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:     0,
		outputTokens:    12,
		cacheReadTokens: 5000,
	})
	if usage.InputTokens != 5000 {
		t.Fatalf("input tokens = %d, want 5000", usage.InputTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 0 {
		t.Fatalf("uncached tokens = %v, want a reported zero", usage.Input.UncachedTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestRawUsageCanonicalIgnoresNegativeCounters pins the malformed-wire
// path: a negative bucket is not a measurement and must not shrink the
// inclusive prompt total.
func TestRawUsageCanonicalIgnoresNegativeCounters(t *testing.T) {
	usage := rawUsageCanonical(rawUsage{
		inputTokens:     1000,
		outputTokens:    40,
		cacheReadTokens: -5,
	})
	if usage.InputTokens != 1000 || usage.TotalTokens != 1040 {
		t.Fatalf("usage = %+v, want 1000 in / 1040 total", usage)
	}
	if usage.Input.CacheReadTokens != nil {
		t.Fatalf("cache read tokens = %v, want none reported",
			usage.Input.CacheReadTokens)
	}
	if usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 1000 {
		t.Fatalf("uncached tokens = %v, want 1000", usage.Input.UncachedTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// A negative write total with a positive TTL split must not keep a
	// breakdown that disagrees with the reported total.
	write := rawUsageCanonical(rawUsage{
		inputTokens:      100,
		outputTokens:     5,
		cacheWriteTokens: -3,
		cacheWrite5m:     7,
	})
	if write.Input.CacheWriteTokens != nil || write.Input.CacheWrites != nil {
		t.Fatalf("write breakdown = %+v, want it dropped", write.Input)
	}
	if err := write.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
