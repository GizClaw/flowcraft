// Package telemetry provides OpenTelemetry initialization and global accessor
// functions for traces, metrics, and logs.
//
// Three pipelines are initialised independently via InitTracer, InitMeter, and
// InitLog. When no exporter is configured the SDK is still installed so that
// valid trace/span IDs are available for structured log correlation, while
// actual export is discarded (zero overhead).
package telemetry

import (
	"strings"

	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
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
	return otel.Tracer(InstrumentationName)
}

// TracerWithSuffix returns a named sub-tracer (e.g. "flowcraft/store").
func TracerWithSuffix(suffix string) trace.Tracer {
	suffix = strings.TrimSpace(suffix)
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		return Tracer()
	}
	return otel.Tracer(InstrumentationName + "/" + suffix)
}

// Meter returns a meter scoped to the framework.
func Meter() metric.Meter {
	return otel.Meter(InstrumentationName)
}

// MeterWithSuffix returns a named sub-meter.
func MeterWithSuffix(suffix string) metric.Meter {
	suffix = strings.TrimSpace(suffix)
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		return Meter()
	}
	return otel.Meter(InstrumentationName + "/" + suffix)
}

// Logger returns an OpenTelemetry Logger from the global LoggerProvider.
func Logger(name string) otellog.Logger {
	name = strings.TrimSpace(name)
	if name == "" {
		name = InstrumentationName + "/logs"
	}
	return logglobal.Logger(name)
}
