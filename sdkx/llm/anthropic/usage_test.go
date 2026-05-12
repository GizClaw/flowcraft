package anthropic

import "testing"

// TestNormalizeAnthropicUsage pins the three-bucket → (gross, cached)
// arithmetic. Anthropic's wire format reports new / cache-read /
// cache-creation as independent counters; the adapter must sum all
// three into `gross` (matching OpenAI's `prompt_tokens` semantics)
// and surface the cache-read subset alone as `cached`. A regression
// here would silently under-count InputTokens for every Anthropic /
// Minimax call, breaking budget enforcement and cache hit-rate
// dashboards everywhere downstream — so we lock the contract in a
// dedicated unit test rather than only exercising it via the
// (env-gated, slow) conformance suite.
func TestNormalizeAnthropicUsage(t *testing.T) {
	tests := []struct {
		name                                                        string
		inputTokens, cacheReadInputTokens, cacheCreationInputTokens int64
		wantGross, wantCached                                       int64
	}{
		{
			name:                     "cold call: only new input, no cache buckets",
			inputTokens:              1000,
			cacheReadInputTokens:     0,
			cacheCreationInputTokens: 0,
			wantGross:                1000,
			wantCached:               0,
		},
		{
			name:                     "first call that writes cache: new + creation, no read",
			inputTokens:              200,
			cacheReadInputTokens:     0,
			cacheCreationInputTokens: 800,
			wantGross:                1000,
			wantCached:               0,
		},
		{
			name:                     "warm hit: new tail + cached prefix, no creation",
			inputTokens:              50,
			cacheReadInputTokens:     950,
			cacheCreationInputTokens: 0,
			wantGross:                1000,
			wantCached:               950,
		},
		{
			name:                     "mixed: cache extended on warm call (read + extra creation)",
			inputTokens:              50,
			cacheReadInputTokens:     900,
			cacheCreationInputTokens: 200,
			wantGross:                1150,
			wantCached:               900,
		},
		{
			name:                     "zero usage (e.g. error response with empty counters) stays zero",
			inputTokens:              0,
			cacheReadInputTokens:     0,
			cacheCreationInputTokens: 0,
			wantGross:                0,
			wantCached:               0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotGross, gotCached := normalizeAnthropicUsage(tt.inputTokens, tt.cacheReadInputTokens, tt.cacheCreationInputTokens)
			if gotGross != tt.wantGross || gotCached != tt.wantCached {
				t.Errorf("normalizeAnthropicUsage(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.inputTokens, tt.cacheReadInputTokens, tt.cacheCreationInputTokens,
					gotGross, gotCached, tt.wantGross, tt.wantCached)
			}
			// Contract invariant: cached <= gross. A regression that
			// e.g. accidentally subtracts cache_read would violate
			// this and break every downstream "hit-rate ≤ 1.0" check.
			if gotCached > gotGross {
				t.Errorf("invariant violated: cached %d > gross %d", gotCached, gotGross)
			}
		})
	}
}
