package telemetry_test

import (
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricembedded "go.opentelemetry.io/otel/metric/embedded"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	traceembedded "go.opentelemetry.io/otel/trace/embedded"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// The cache's contract is per provider: one instrument per scope while the global
// provider stays put, and a rebuild when it is replaced. A test that only asserted "the
// same value came back" would pass on a cache that ignored the provider, so the probes
// below hand out a distinct instrument per build and count the builds per scope.
//
// Counted per scope because the count is not only ours: when otel.SetTracerProvider
// takes a delegate, the global provider re-resolves the tracers it already handed out —
// the ones another test cached — so a raw build count on the new provider starts above
// one. Only the scope a test asks for is evidence about the cache.

var (
	tracerSink trace.Tracer
	meterSink  metric.Meter
)

// probeTracerProvider counts the tracers it hands out, per scope.
type probeTracerProvider struct {
	traceembedded.TracerProvider
	mu    sync.Mutex
	built map[string]int
}

func (p *probeTracerProvider) Tracer(name string, _ ...trace.TracerOption) trace.Tracer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.built == nil {
		p.built = make(map[string]int, 1)
	}
	p.built[name]++
	return &probeTracer{Tracer: tracenoop.NewTracerProvider().Tracer(name), provider: p}
}

func (p *probeTracerProvider) builds(scope string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.built[scope]
}

// probeTracer is a tracer that is only itself: two builds never compare equal, which is
// what lets a test see a rebuild through the trace.Tracer interface.
type probeTracer struct {
	trace.Tracer
	provider *probeTracerProvider
}

// probeMeterProvider counts the meters it hands out, for the same reason.
type probeMeterProvider struct {
	metricembedded.MeterProvider
	mu    sync.Mutex
	built map[string]int
}

func (p *probeMeterProvider) Meter(name string, _ ...metric.MeterOption) metric.Meter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.built == nil {
		p.built = make(map[string]int, 1)
	}
	p.built[name]++
	return &probeMeter{Meter: metricnoop.NewMeterProvider().Meter(name), provider: p}
}

func (p *probeMeterProvider) builds(scope string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.built[scope]
}

type probeMeter struct {
	metric.Meter
	provider *probeMeterProvider
}

// TestTracerWithSuffixCachesOneTracerPerProvider is the tracer half of the contract:
// repeated resolutions of one suffix build one tracer, and installing a provider builds
// the next one from the new provider instead of returning the old one.
func TestTracerWithSuffixCachesOneTracerPerProvider(t *testing.T) {
	const suffix, scope = "cache-probe-tracer", "flowcraft/cache-probe-tracer"

	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	first := &probeTracerProvider{}
	otel.SetTracerProvider(first)

	cached := telemetry.TracerWithSuffix(suffix)
	for range 8 {
		if got := telemetry.TracerWithSuffix(suffix); got != cached {
			t.Fatalf("TracerWithSuffix = %v, want the cached tracer", got)
		}
	}
	// One scope, however it is spelled: the trimmed suffix is the cache key.
	if got := telemetry.TracerWithSuffix("  /" + suffix + "/  "); got != cached {
		t.Fatalf("TracerWithSuffix(/%s/) = %v, want the cached tracer", suffix, got)
	}
	if got := first.builds(scope); got != 1 {
		t.Fatalf("first provider built %d tracers for %q, want 1", got, scope)
	}

	second := &probeTracerProvider{}
	otel.SetTracerProvider(second)

	rebuilt := telemetry.TracerWithSuffix(suffix)
	if got := second.builds(scope); got != 1 {
		t.Fatalf("second provider built %d tracers for %q after the swap, want 1", got, scope)
	}
	if rebuilt == cached {
		t.Fatal("the tracer built from the replaced provider was returned after the swap")
	}
	if got := telemetry.TracerWithSuffix(suffix); got != rebuilt {
		t.Fatalf("TracerWithSuffix = %v, want the tracer of the installed provider", got)
	}
	if got := second.builds(scope); got != 1 {
		t.Fatalf("second provider built %d tracers for %q, want 1", got, scope)
	}
	if got := first.builds(scope); got != 1 {
		t.Fatalf("the replaced provider rebuilt %q after the swap (%d builds)", scope, got)
	}
}

// TestMeterWithSuffixCachesOneMeterPerProvider is the meter half: metrics are resolved on
// every recorded call, so the same provider-keyed cache has to back them.
func TestMeterWithSuffixCachesOneMeterPerProvider(t *testing.T) {
	const suffix, scope = "cache-probe-meter", "flowcraft/cache-probe-meter"

	previous := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	first := &probeMeterProvider{}
	otel.SetMeterProvider(first)

	cached := telemetry.MeterWithSuffix(suffix)
	for range 8 {
		if got := telemetry.MeterWithSuffix(suffix); got != cached {
			t.Fatalf("MeterWithSuffix = %v, want the cached meter", got)
		}
	}
	if got := telemetry.MeterWithSuffix(" /" + suffix + "/ "); got != cached {
		t.Fatalf("MeterWithSuffix(/%s/) = %v, want the cached meter", suffix, got)
	}
	if got := first.builds(scope); got != 1 {
		t.Fatalf("first provider built %d meters for %q, want 1", got, scope)
	}

	second := &probeMeterProvider{}
	otel.SetMeterProvider(second)

	rebuilt := telemetry.MeterWithSuffix(suffix)
	if got := second.builds(scope); got != 1 {
		t.Fatalf("second provider built %d meters for %q after the swap, want 1", got, scope)
	}
	if rebuilt == cached {
		t.Fatal("the meter built from the replaced provider was returned after the swap")
	}
	if got := telemetry.MeterWithSuffix(suffix); got != rebuilt {
		t.Fatalf("MeterWithSuffix = %v, want the meter of the installed provider", got)
	}
	if got := second.builds(scope); got != 1 {
		t.Fatalf("second provider built %d meters for %q, want 1", got, scope)
	}
	if got := first.builds(scope); got != 1 {
		t.Fatalf("the replaced provider rebuilt %q after the swap (%d builds)", scope, got)
	}
}

// TestWithSuffixCachedPathDoesNotAllocate guards the reason the cache exists: a layer that
// resolves its instrument per call must not allocate for it once the entry is warm.
func TestWithSuffixCachedPathDoesNotAllocate(t *testing.T) {
	previousTracer := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previousTracer) })
	previousMeter := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previousMeter) })

	otel.SetTracerProvider(&probeTracerProvider{})
	otel.SetMeterProvider(&probeMeterProvider{})

	tracerSink = telemetry.TracerWithSuffix("alloc-probe")
	meterSink = telemetry.MeterWithSuffix("alloc-probe")
	allocs := testing.AllocsPerRun(100, func() {
		tracerSink = telemetry.TracerWithSuffix("alloc-probe")
		meterSink = telemetry.MeterWithSuffix("alloc-probe")
	})
	if allocs != 0 {
		t.Fatalf("resolving a cached tracer and meter allocates %.0f times per call", allocs)
	}
}
