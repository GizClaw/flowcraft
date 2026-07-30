package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	toolMeter = telemetry.MeterWithSuffix("tool")

	toolExecCount, _    = toolMeter.Int64Counter("executions.total", metric.WithDescription("Total tool executions"))
	toolExecDuration, _ = toolMeter.Float64Histogram("duration.seconds", metric.WithDescription("Tool execution duration"))
	toolErrorCount, _   = toolMeter.Int64Counter("errors.total", metric.WithDescription("Total tool execution errors"))
)

// Telemetry wraps dispatch with the standard observability stack:
// an OTel span per call, execution/duration/error metrics, and a
// warning log for failed calls. Failure is read off Result.IsError,
// so it sees tool errors, policy short-circuits from inner
// middleware, and unknown-tool lookups alike.
func Telemetry() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call tool.Call) tool.Result {
			ctx, span := telemetry.Tracer().Start(ctx,
				fmt.Sprintf("tool.%s.execute", call.Name),
				trace.WithAttributes(attribute.String(telemetry.AttrToolName, call.Name)))
			defer span.End()

			nameAttr := metric.WithAttributes(
				attribute.String(telemetry.AttrToolName, call.Name))

			start := time.Now()
			res := next(ctx, call)
			dur := time.Since(start)

			span.SetAttributes(attribute.Float64("tool.duration_s", dur.Seconds()))
			toolExecDuration.Record(ctx, dur.Seconds(), nameAttr)

			if res.IsError {
				span.SetStatus(codes.Error, res.Content)
				toolExecCount.Add(ctx, 1, metric.WithAttributes(
					attribute.String(telemetry.AttrToolName, call.Name),
					attribute.String("status", "error")))
				toolErrorCount.Add(ctx, 1, nameAttr)
				telemetry.Warn(ctx, "tool execution failed",
					otellog.String(telemetry.AttrToolName, call.Name),
					otellog.String(telemetry.AttrErrorMessage, res.Content))
				return res
			}

			span.SetStatus(codes.Ok, "OK")
			toolExecCount.Add(ctx, 1, metric.WithAttributes(
				attribute.String(telemetry.AttrToolName, call.Name),
				attribute.String("status", "success")))
			return res
		}
	}
}
