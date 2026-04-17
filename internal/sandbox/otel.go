package sandbox

import (
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/metric"
)

var (
	sbMeter = telemetry.MeterWithSuffix("sandbox")

	sbExecCount, _    = sbMeter.Int64Counter("executions.total", metric.WithDescription("Total sandbox command executions"))
	sbExecDuration, _ = sbMeter.Float64Histogram("duration.seconds", metric.WithDescription("Sandbox command execution duration"))
	sbActive, _       = sbMeter.Int64UpDownCounter("sandboxes.active", metric.WithDescription("Currently active sandbox instances"))
	sbCreated, _      = sbMeter.Int64Counter("sandboxes.created.total", metric.WithDescription("Total sandbox instances created"))
	sbDestroyed, _    = sbMeter.Int64Counter("sandboxes.destroyed.total", metric.WithDescription("Total sandbox instances destroyed"))
)
