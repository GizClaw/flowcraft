// Package telemetry is the instrumentation half of OpenTelemetry support: the
// accessors that produce spans, metrics and log records, plus the attribute keys
// they standardise on.
//
// It depends on the OpenTelemetry API only, never the SDK or an exporter. That is
// deliberate: a package that logs a warning must not drag a tracing backend into
// the binary. With no provider installed the API's no-ops apply, so every helper
// here is safe to call before a host has wired anything.
//
// Installation lives in the setup subpackage: telemetry/setup builds the providers,
// resources and exporters, and installs them globally. A host imports it once at
// process start; libraries import this package and nothing else.
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
