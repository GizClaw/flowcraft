package sandbox

import (
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/metric"
)

var (
	sbMeter = telemetry.MeterWithSuffix("sandbox")

	sbExecCount, _           = sbMeter.Int64Counter("executions.total", metric.WithDescription("Total sandbox command executions"))
	sbExecDuration, _        = sbMeter.Float64Histogram("duration.seconds", metric.WithDescription("Sandbox command execution duration"))
	sbContainersActive, _    = sbMeter.Int64UpDownCounter("containers.active", metric.WithDescription("Currently active sandbox containers"))
	sbContainersCreated, _   = sbMeter.Int64Counter("containers.created.total", metric.WithDescription("Total sandbox containers created"))
	sbContainersDestroyed, _ = sbMeter.Int64Counter("containers.destroyed.total", metric.WithDescription("Total sandbox containers destroyed"))
)
