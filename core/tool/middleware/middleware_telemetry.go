package middleware

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// spanToolExecute is the one span name every tool call uses. The tool is carried
// in AttrToolName: a name per tool ("bash.execute") gives the name a cardinality
// equal to the catalog and forces every dashboard to filter by name instead of
// grouping by it.
const spanToolExecute = "tool.execute"

// unclassifiedFailure is reported when a call failed but no stage recorded why.
// It is a signal that a short-circuit is missing a tool.RecordFailure call, not
// a category to build on.
const unclassifiedFailure = "unclassified"

// Status values for the metrics' `status` dimension.
const (
	statusSuccess = "success"
	statusError   = "error"
)

// Telemetry wraps dispatch with the standard observability stack: one span per
// call, execution/duration/error metrics, and a warning log for failures.
//
// Failure is read off Result.IsError, and the classification comes from the
// recorder the middleware installs before calling inward: the stages that
// short-circuit a call (approval, timeout, the catalog lookup, panic recovery)
// tag *why* through tool.RecordFailure, so the span never has to parse an error
// message to name a cause.
func Telemetry() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			ctx = tool.WithFailureRecorder(ctx)
			ctx, span := telemetry.TracerWithSuffix("tool").Start(ctx, spanToolExecute,
				trace.WithAttributes(
					attribute.String(telemetry.AttrToolName, call.Name),
					attribute.String(telemetry.AttrToolCallID, call.ID),
				))
			defer span.End()

			start := time.Now()
			res := next(ctx, call)
			duration := time.Since(start)

			if !res.IsError {
				span.SetStatus(codes.Ok, "")
				recordToolMetrics(ctx, call.Name, statusSuccess, "", duration)
				return res
			}

			kind, ok := tool.FailureKindFrom(ctx)
			if !ok {
				kind = unclassifiedFailure
			}
			// The status description is the classification, never the result
			// text: the text can carry command output, paths, or a credential
			// the tool echoed back. The log record keeps the detail.
			span.SetAttributes(attribute.String(telemetry.AttrErrorKind, string(kind)))
			span.SetStatus(codes.Error, string(kind))
			telemetry.Warn(ctx, "tool execution failed",
				attribute.String(telemetry.AttrToolName, call.Name),
				attribute.String(telemetry.AttrToolCallID, call.ID),
				attribute.String(telemetry.AttrErrorKind, string(kind)),
				attribute.String(telemetry.AttrErrorMessage, res.Content.Text()))
			recordToolMetrics(ctx, call.Name, statusError, string(kind), duration)
			return res
		}
	}
}

// annotate sets attributes on the active span, if any.
//
// Middleware nested inside the telemetry span calls this instead of carrying its
// own recorder: Start already put the span in the context we were handed, so a
// policy decision or a rate-limit wait lands on the one span that describes the
// call. With no span in the context (a hand-built chain) the call is free.
func annotate(ctx context.Context, attrs ...attribute.KeyValue) {
	if len(attrs) == 0 {
		return
	}
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// recordToolMetrics emits the per-call series.
//
// Instruments are resolved per call rather than once at package init: the global
// meter provider is swapped in tests, and an instrument created before the swap
// keeps delegating to the provider that existed then. The SDK caches meters by
// scope and instruments by identity, so this is a few map lookups.
func recordToolMetrics(
	ctx context.Context,
	name string,
	status string,
	kind string,
	duration time.Duration,
) {
	meter := telemetry.MeterWithSuffix("tool")
	nameAttr := metric.WithAttributes(attribute.String(telemetry.AttrToolName, name))

	if counter, err := meter.Int64Counter("executions.total",
		metric.WithDescription("Tool executions by outcome")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrToolName, name),
			attribute.String("status", status)))
	}
	if histogram, err := meter.Float64Histogram("duration.seconds",
		metric.WithDescription("Tool execution duration")); err == nil {
		histogram.Record(ctx, duration.Seconds(), nameAttr)
	}
	if kind != "" {
		if counter, err := meter.Int64Counter("errors.total",
			metric.WithDescription("Tool execution failures by classification")); err == nil {
			counter.Add(ctx, 1, metric.WithAttributes(
				attribute.String(telemetry.AttrToolName, name),
				attribute.String(telemetry.AttrErrorKind, kind)))
		}
	}
}
