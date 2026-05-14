package llm_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// pkgMetricReader is the package-level OTel ManualReader installed
// once via TestMain. It exists because this test asserts on
// llm.RecordLLMMetrics counters as the regression surface for the
// streaming half of the LLM API (sdkx/llm/{openai,anthropic,
// bytedance}/stream.go → finish() → RecordLLMMetrics).
//
// Historical note: stream.Usage() used to return a minimal
// Usage{Input,Output} that did not surface CachedInputTokens, so
// the OTel counter was the only public observable for stream-path
// cache stats. As of #136 the model.Usage struct carries
// CachedInputTokens too, but this conformance module is pinned to
// an older sdk version and still treats the counter as authoritative.
// Once the module bumps sdk past #136, an additional
// stream.Usage().CachedInputTokens assertion is straightforward to
// add as a second, cheaper regression layer.
//
// The OTel global meter delegation rebinds package-level instruments
// only on the FIRST SetMeterProvider call per process (see
// sdk/llm/metrics_test.go for the same caveat), so we install the
// reader exactly once in TestMain and let every Test* in this
// package observe through it. A per-test installer would silently
// leave the second-and-later test bound to the first test's
// already-shutdown provider — masking real metric regressions
// behind "ok" runs.
var pkgMetricReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	pkgMetricReader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(pkgMetricReader))
	otel.SetMeterProvider(provider)
	code := m.Run()
	_ = provider.Shutdown(context.Background())
	os.Exit(code)
}

// counterSumByProvider returns the running cumulative value of the
// named int64 counter, restricted to data points whose `provider`
// attribute matches providerName. We MUST scope by attribute (not
// take the global sum) because conformance tests share the same
// process and therefore the same OTel reader: a per-test "before /
// after" delta computed against the global sum would conflate
// unrelated calls (e.g. an earlier qwen warm-up call leaking into a
// later azure assertion). Filtering by provider gives every
// sub-test a private slice of counter state.
//
// Tests take a "before" / "after" pair at the same provider scope
// and assert on the diff so a fresh ManualReader (no data) and an
// already-warmed reader produce the same delta semantics.
func counterSumByProvider(t *testing.T, name, providerName string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := pkgMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("provider")); ok && v.AsString() == providerName {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestProviders_PromptCacheHitRate_Stream is the streaming-path
// counterpart of TestProviders_PromptCacheHitRate. Why it exists:
// the synchronous test exercises adapter.Generate, but production
// traffic in the framework's primary callers (sdk/agent, copilot
// dispatcher, run-loop nodes) flows through GenerateStream. The
// per-adapter stream finish() functions latch CachedInputTokens
// through a separate code path than the synchronous adapter.Generate,
// so a regression in the stream-side latching would not be caught by
// TestProviders_PromptCacheHitRate.
//
// Specifically, the gaps users have hit in production:
//
//   - sdkx/llm/openai/stream.go latches
//     chunk.Usage.PromptTokensDetails.CachedTokens onto
//     s.usage.CachedInputTokens, then finish() packages it into
//     llm.TokenUsage.CachedInputTokens for RecordLLMMetrics. If any
//     step regresses (SDK rename, refactor drops the plumbing,
//     finish() forgets to read the field), the synchronous Generate
//     path remains unaffected — but the cached-tokens metric
//     vanishes for every streaming caller.
//   - sdkx/llm/anthropic/stream.go has TWO finish paths (stable +
//     beta JSON-mode) each with their own latching site, doubling
//     the surface a sync-only test would miss.
//   - sdkx/llm/bytedance/stream.go finish() previously dropped
//     RecordLLMMetrics entirely until the cached-tokens-telemetry
//     PR added it back; that bug shipped to production undetected
//     because no stream-path metric coverage existed.
//
// We observe via the OTel ManualReader (counter) because this
// conformance module is pinned to an older sdk version whose
// model.Usage did not expose CachedInputTokens — the metric was the
// only public observable. Post-#136 the streaming Usage() also
// carries the field, so once this module's sdk pin is bumped a
// second assertion on stream.Usage().CachedInputTokens is a cheap
// way to catch adapter-side regressions before they reach the
// telemetry boundary.
func TestProviders_PromptCacheHitRate_Stream(t *testing.T) {
	stableSystem := buildStableSystemPrompt()

	for _, spec := range cachedTokensProviders {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			// Pre-warm metric snapshot. Any earlier sub-test against
			// the same provider would have pushed the counter up;
			// taking the snapshot AFTER createProvider lets us
			// measure only the deltas attributable to this run's
			// warm-up + follow-up calls.
			beforeCached := counterSumByProvider(t, "tokens.input.cached.total", spec.Provider)
			beforeInput := counterSumByProvider(t, "tokens.input.total", spec.Provider)

			baseMsgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, stableSystem),
				llm.NewTextMessage(llm.RoleUser, "Step 1 of 2: respond with the literal word OK."),
			}

			// Warm-up streaming call. Most providers will report
			// cached=0 here (cold cache); we drain the stream the
			// same way production code does to make sure
			// cachedInputTokens latches in the chunk-by-chunk path,
			// not just the final-frame fast path.
			s1, err := provider.GenerateStream(ctx, baseMsgs, llm.WithMaxTokens(20), llm.WithTemperature(0.1))
			if err != nil {
				t.Fatalf("warm-up GenerateStream failed: %v", err)
			}
			drainStream(t, s1, "warm-up")

			// Follow-up streaming call with identical system prefix.
			// Same delta semantics as the synchronous version: a
			// multi-KB stable prefix dominates the few new user
			// tokens, so the cache hit rate must be high.
			hitMsgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, stableSystem),
				llm.NewTextMessage(llm.RoleUser, "Step 2 of 2: respond with the literal word DONE."),
			}
			s2, err := provider.GenerateStream(ctx, hitMsgs, llm.WithMaxTokens(20), llm.WithTemperature(0.1))
			if err != nil {
				t.Fatalf("follow-up GenerateStream failed: %v", err)
			}
			drainStream(t, s2, "follow-up")

			afterCached := counterSumByProvider(t, "tokens.input.cached.total", spec.Provider)
			afterInput := counterSumByProvider(t, "tokens.input.total", spec.Provider)

			deltaCached := afterCached - beforeCached
			deltaInput := afterInput - beforeInput

			t.Logf("stream metrics delta: input=%d cached=%d (hit-rate=%.1f%%)",
				deltaInput, deltaCached, ratePct(deltaCached, deltaInput))

			// Hard contract — without input we cannot draw any
			// conclusion about cache. If this trips, the stream
			// finish path silently dropped RecordLLMMetrics
			// entirely (the historical bytedance/stream.go bug
			// before sdk v0.3.6 / sdkx v0.3.4) and the cached
			// assertion below would be meaningless.
			if deltaInput == 0 {
				t.Fatalf("tokens.input.total delta=0 across two streaming calls: stream.finish() did not call RecordLLMMetrics — the entire stream metric pipeline is dead")
			}

			// Strict contract for adapters where we drive caching
			// explicitly (anthropic / minimax via cache_control,
			// bytedance via transparent prefix caching documented
			// reliable for Doubao). For the OpenAI-family implicit
			// caching the threshold can be flaky on quieter
			// regions / smaller SKUs, so we only warn there to
			// stay symmetric with the synchronous test.
			strictHit := spec.Provider == "anthropic" || spec.Provider == "minimax" || spec.Provider == "bytedance"
			if strictHit && deltaCached == 0 {
				t.Errorf("tokens.input.cached.total delta=0 for stream calls on %s — stream finish path is not plumbing CachedInputTokens through llm.RecordLLMMetrics. This is the metric the user reported missing.", spec.Provider)
			}
			if !strictHit && deltaCached == 0 {
				t.Logf("WARN: provider %s stream cached counter unchanged. Could be (a) cold region, (b) prompt below provider minimum, or (c) stream finish regression. Investigate before claiming optimisation works on the streaming path.",
					spec.Provider)
			}

			// Invariant: cached subset cannot exceed gross input on
			// the metric pipeline either — symmetric with the live
			// per-call invariant in the synchronous test.
			if deltaCached > deltaInput {
				t.Errorf("metric invariant violated: cached %d > input %d (stream pipeline mis-attributing buckets)", deltaCached, deltaInput)
			}
		})
	}
}

// drainStream reads every chunk from a streaming response and runs
// the standard error / Close cleanup. Extracted so the warm-up and
// follow-up halves of the cache-hit assertion share an identical
// drain shape — a divergent drain (e.g. one path skipping Close())
// would only surface the metric-pipeline bug we are trying to test
// half the time and waste a flaky-looking failure on the other.
func drainStream(t *testing.T, s llm.StreamMessage, phase string) {
	t.Helper()
	defer func() {
		if err := s.Close(); err != nil {
			t.Logf("%s stream Close: %v", phase, err)
		}
	}()
	var buf strings.Builder
	for s.Next() {
		buf.WriteString(s.Current().Content)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("%s stream error: %v", phase, err)
	}
	u := s.Usage()
	t.Logf("%s drained: chars=%d input=%d output=%d", phase, buf.Len(), u.InputTokens, u.OutputTokens)
}
