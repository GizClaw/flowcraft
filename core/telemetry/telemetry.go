package telemetry

import (
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	InstrumentationName = "flowcraft"
	ServiceName         = "flowcraft"
	ServiceVersion      = "0.1.0"
)

// Tracer returns a tracer scoped to the framework.
func Tracer() trace.Tracer {
	return tracerCache.get(otel.GetTracerProvider(), "", func() trace.Tracer {
		return otel.Tracer(InstrumentationName)
	})
}

// TracerWithSuffix returns a named sub-tracer (e.g. "flowcraft/store").
func TracerWithSuffix(suffix string) trace.Tracer {
	suffix = normalizeSuffix(suffix)
	if suffix == "" {
		return Tracer()
	}
	return tracerCache.get(otel.GetTracerProvider(), suffix, func() trace.Tracer {
		return otel.Tracer(InstrumentationName + "/" + suffix)
	})
}

// Meter returns a meter scoped to the framework.
func Meter() metric.Meter {
	return meterCache.get(otel.GetMeterProvider(), "", func() metric.Meter {
		return otel.Meter(InstrumentationName)
	})
}

// MeterWithSuffix returns a named sub-meter.
func MeterWithSuffix(suffix string) metric.Meter {
	suffix = normalizeSuffix(suffix)
	if suffix == "" {
		return Meter()
	}
	return meterCache.get(otel.GetMeterProvider(), suffix, func() metric.Meter {
		return otel.Meter(InstrumentationName + "/" + suffix)
	})
}

// normalizeSuffix trims what a caller may have written (" /store/ ") down to the label the
// scope name carries, so two spellings of one suffix cannot occupy two cache entries.
func normalizeSuffix(suffix string) string {
	suffix = strings.TrimSpace(suffix)
	return strings.Trim(suffix, "/")
}

// The caches. A producer resolves its tracer or meter where it needs it — the tool
// registry and the agent metrics ask on every call — because holding one in a
// package-level variable would capture whichever provider happened to be installed when
// the package initialised, and a test that installs its own provider afterwards would then
// silently emit nothing. otel.Tracer and otel.Meter are cheap but not free: they build the
// scope name, walk the provider and, on an SDK provider, allocate a wrapper per call.
//
// So the resolution is memoized per scope, keyed by the identity of the global provider. A
// process that installs its provider once pays one interface comparison and a map lookup
// per call and allocates nothing. A test (or a host) that calls otel.SetTracerProvider
// changes that identity, which drops the cache; the next call rebuilds, so an instrument
// resolved after the swap binds to the provider in force then — the property the
// instrumentation kit and every test that installs a provider depend on.
var (
	tracerCache providerCache[trace.TracerProvider, trace.Tracer]
	meterCache  providerCache[metric.MeterProvider, metric.Meter]
)

// providerCache memoizes the values built for one provider, keyed by the caller's own key.
type providerCache[P comparable, T any] struct {
	mu       sync.RWMutex
	provider P
	entries  map[string]T
}

// get returns the entry for key, building it with build when the provider is new or the key
// is not cached yet.
func (c *providerCache[P, T]) get(provider P, key string, build func() T) T {
	c.mu.RLock()
	// The comparison is the point: while the provider is unchanged, this and one map
	// lookup are the whole cost of resolving an instrument.
	if c.provider == provider {
		if value, ok := c.entries[key]; ok {
			c.mu.RUnlock()
			return value
		}
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-read under the write lock: another goroutine may have filled the entry, or the
	// provider may have been replaced while this one waited for it.
	if c.provider != provider {
		c.entries = nil
		c.provider = provider
	}
	if value, ok := c.entries[key]; ok {
		return value
	}
	value := build()
	if c.entries == nil {
		c.entries = make(map[string]T, 1)
	}
	c.entries[key] = value
	return value
}

// Logger returns an OpenTelemetry Logger from the global LoggerProvider.
func Logger(name string) otellog.Logger {
	name = strings.TrimSpace(name)
	if name == "" {
		name = InstrumentationName + "/logs"
	}
	return otel.Logger(name)
}
