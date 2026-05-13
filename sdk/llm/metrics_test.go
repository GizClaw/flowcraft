package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// pkgMetricReader is the package-level ManualReader used by every
// metric test below. It is installed exactly once via TestMain because
// the OpenTelemetry Go global meter delegation rebinds package-level
// instruments only on the FIRST SetMeterProvider call per process
// (see internal/global.delegateMeterOnce). A per-test installer would
// silently leave the second-and-later test bound to the first test's
// (already-shutdown) meter provider, masking real metric regressions
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

// counterDelta is a snapshot helper that returns the cumulative
// counter value for `name` in the current ResourceMetrics. Tests
// take a "before" / "after" pair and assert on the diff so the
// shared package-wide reader's accumulated state across earlier
// tests does not bleed into the assertions.
type counterSnapshot struct {
	values map[string]int64 // name -> running sum across all data points
}

func snapshotCounters(t *testing.T) counterSnapshot {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := pkgMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	out := counterSnapshot{values: make(map[string]int64)}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			out.values[m.Name] = total
		}
	}
	return out
}

// delta returns after.values[name] - before.values[name], with the
// "missing" sentinel mapped to zero so a "no data point ever
// emitted" case is observable as `delta == 0` (the test then has
// to decide whether that is the desired behaviour).
func delta(before, after counterSnapshot, name string) int64 {
	return after.values[name] - before.values[name]
}

func TestRecordLLMMetrics_EmitsCachedTokensCounter(t *testing.T) {
	before := snapshotCounters(t)
	ctx := context.Background()

	RecordLLMMetrics(ctx, "openai", "gpt-4o", "success", 100*time.Millisecond, TokenUsage{
		InputTokens:       1000,
		OutputTokens:      200,
		TotalTokens:       1200,
		CachedInputTokens: 800,
	})

	after := snapshotCounters(t)

	if got := delta(before, after, "tokens.input.cached.total"); got != 800 {
		t.Errorf("tokens.input.cached.total delta = %d, want 800 — counter is not wired into RecordLLMMetrics", got)
	}
	if got := delta(before, after, "tokens.input.total"); got != 1000 {
		t.Errorf("tokens.input.total delta = %d, want 1000", got)
	}
	if got := delta(before, after, "tokens.output.total"); got != 200 {
		t.Errorf("tokens.output.total delta = %d, want 200", got)
	}
	if got := delta(before, after, "requests.total"); got != 1 {
		t.Errorf("requests.total delta = %d, want 1", got)
	}
}

func TestRecordLLMMetrics_OmitsCachedCounterWhenZero(t *testing.T) {
	// Provider that did not surface a cache breakdown — no NEW data
	// point on the cached counter must appear, otherwise dashboards
	// would see a fake "0% cache hit" row for every cache-unaware
	// provider call.
	before := snapshotCounters(t)
	ctx := context.Background()

	RecordLLMMetrics(ctx, "ollama", "qwen2.5:7b", "success", 50*time.Millisecond, TokenUsage{
		InputTokens:  500,
		OutputTokens: 100,
		TotalTokens:  600,
		// CachedInputTokens left zero on purpose.
	})

	after := snapshotCounters(t)

	if got := delta(before, after, "tokens.input.cached.total"); got != 0 {
		t.Errorf("tokens.input.cached.total delta = %d, want 0 (CachedInputTokens=0 must not produce a data point)", got)
	}
	// Sanity: the regular counters DID move — without this guard the
	// cached-omitted assertion would pass even if the metric pipe
	// was silently dropping every metric (a useless test).
	if got := delta(before, after, "tokens.input.total"); got != 500 {
		t.Errorf("tokens.input.total delta = %d, want 500", got)
	}
	if got := delta(before, after, "tokens.output.total"); got != 100 {
		t.Errorf("tokens.output.total delta = %d, want 100", got)
	}
}

func TestRecordLLMMetrics_OmitsAllTokenCountersOnZeroInput(t *testing.T) {
	// Error fall-through paths in the adapters call RecordLLMMetrics
	// with a zero TokenUsage. The token counters MUST stay silent
	// in that case (the request counter handles error rate).
	before := snapshotCounters(t)
	ctx := context.Background()

	RecordLLMMetrics(ctx, "openai", "gpt-4o", "error", 10*time.Millisecond, TokenUsage{})

	after := snapshotCounters(t)

	for _, name := range []string{
		"tokens.input.total",
		"tokens.output.total",
		"tokens.input.cached.total",
	} {
		if got := delta(before, after, name); got != 0 {
			t.Errorf("%s delta = %d, want 0 (zero TokenUsage / error path)", name, got)
		}
	}
	// requests.total must still record the error so error-rate dashboards work.
	if got := delta(before, after, "requests.total"); got != 1 {
		t.Errorf("requests.total delta = %d, want 1 even on error path", got)
	}
}

func TestUsageSpanAttrs_AlwaysEmitsInputOutput(t *testing.T) {
	attrs := UsageSpanAttrs(TokenUsage{InputTokens: 0, OutputTokens: 0})

	wantKeys := map[string]int64{
		telemetry.AttrLLMInputTokens:  0,
		telemetry.AttrLLMOutputTokens: 0,
	}
	if len(attrs) != len(wantKeys) {
		t.Fatalf("UsageSpanAttrs(zero usage) returned %d attrs, want %d (input + output, no cached)", len(attrs), len(wantKeys))
	}
	for _, kv := range attrs {
		want, ok := wantKeys[string(kv.Key)]
		if !ok {
			t.Errorf("unexpected attr %q in zero-usage attribute slice", kv.Key)
			continue
		}
		if kv.Value.AsInt64() != want {
			t.Errorf("attr %q = %d, want %d", kv.Key, kv.Value.AsInt64(), want)
		}
	}
}

func TestUsageSpanAttrs_EmitsCachedOnlyWhenPositive(t *testing.T) {
	t.Run("cached omitted when zero", func(t *testing.T) {
		attrs := UsageSpanAttrs(TokenUsage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 0})
		for _, kv := range attrs {
			if string(kv.Key) == telemetry.AttrLLMCachedInputTokens {
				t.Errorf("cached attr should be omitted when CachedInputTokens=0, got %v", kv)
			}
		}
	})
	t.Run("cached present when positive", func(t *testing.T) {
		attrs := UsageSpanAttrs(TokenUsage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 90})
		var found *attribute.KeyValue
		for i, kv := range attrs {
			if string(kv.Key) == telemetry.AttrLLMCachedInputTokens {
				found = &attrs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("cached attr missing despite CachedInputTokens=90")
		}
		if found.Value.AsInt64() != 90 {
			t.Errorf("cached attr = %d, want 90", found.Value.AsInt64())
		}
	})
}
