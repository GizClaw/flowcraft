package route

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// This file instruments the dispatch loop: one span per routed request, one event
// per attempt.
//
// The route span answers "did we fall back, how often, and why" — a question the
// per-attempt spans cannot answer, because each of them only knows its own
// outcome. It deliberately carries no gen_ai.* attributes: a builder that groups
// LLM traffic by gen_ai.* would count a failover as two model calls. The attempt
// spans own the gen_ai vocabulary; this one owns route.*.
//
// The span ends when the dispatch loop returns — including for streams, where it
// ends when the stream opens. Whether the stream then succeeds is the attempt
// span's story, and its span lives until the stream's terminal event.

// Operation tokens for the inference.operation dimension.
const (
	operationGenerate = "generate"
	operationDecide   = "decide"
)

// Status values for the metrics' `status` dimension.
const (
	statusSuccess = "success"
	statusError   = "error"
)

// routeCall accumulates one dispatch's attempts and closes its span.
type routeCall struct {
	ctx       context.Context
	span      trace.Span
	start     time.Time
	operation string

	attempts  int
	retries   int
	fallbacks int
	selected  string
	executed  string
	lastKind  string
}

// startRoute opens the route span.
func startRoute(ctx context.Context, operation string) (context.Context, *routeCall) {
	ctx, span := telemetry.TracerWithSuffix("inference/route").Start(ctx, "llm.route",
		trace.WithAttributes(
			attribute.String(telemetry.AttrInferenceOperation, operation),
		))
	return ctx, &routeCall{
		ctx:       ctx,
		span:      span,
		start:     time.Now(),
		operation: operation,
	}
}

// selectTargets records the ordered candidate list the selector returned.
func (c *routeCall) selectTargets(targets []Target) {
	if len(targets) > 0 {
		c.selected = targets[0].Name
	}
}

// attempt records one provider attempt, successful or not. The success event
// matters as much as the failures: it is the only place the trace says which
// candidate answered and how many were skipped before it.
func (c *routeCall) attempt(target Target, number int, backoff time.Duration, err error) {
	c.attempts++
	if number > 1 {
		c.retries++
	}

	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrRouteTarget, target.Name),
		attribute.Int(telemetry.AttrRouteAttemptNumber, number),
	}
	if backoff > 0 {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrRouteBackoffMs, backoff.Milliseconds()))
	}
	if err == nil {
		c.executed = target.Name
		attrs = append(attrs,
			attribute.String(telemetry.AttrRouteOutcome, statusSuccess))
	} else {
		kind := errorKind(err)
		c.lastKind = kind
		attrs = append(attrs,
			attribute.String(telemetry.AttrRouteOutcome, statusError),
			attribute.String(telemetry.AttrErrorKind, kind))
	}
	c.span.AddEvent("route.attempt", trace.WithAttributes(attrs...))
}

// fallback records a move to another candidate.
func (c *routeCall) fallback() {
	c.fallbacks++
}

// finish closes the span, records the route shape and emits the metrics.
func (c *routeCall) finish(err error) {
	duration := time.Since(c.start)
	status := statusSuccess
	kind := ""
	if err != nil {
		status = statusError
		kind = errorKind(err)
	}

	attrs := []attribute.KeyValue{
		attribute.Int(telemetry.AttrRouteAttempts, c.attempts),
		attribute.Int(telemetry.AttrRouteRetries, c.retries),
	}
	if c.fallbacks > 0 {
		attrs = append(attrs,
			attribute.Int(telemetry.AttrRouteFallbacks, c.fallbacks))
	}
	if c.selected != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrRouteSelected, c.selected))
	}
	if c.executed != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrRouteExecuted, c.executed))
	}
	if err != nil {
		attrs = append(attrs,
			attribute.String(telemetry.AttrErrorKind, kind))
		c.span.SetStatus(codes.Error, kind)
	} else {
		c.span.SetStatus(codes.Ok, "")
	}
	c.span.SetAttributes(attrs...)

	c.recordMetrics(duration, status, kind)
	c.span.End()
}

// recordMetrics emits the routing series. Instruments are resolved per call for
// the same reason as the inference funnel: the global meter provider is swapped
// in tests, and the SDK caches the meters and instruments behind the lookup.
func (c *routeCall) recordMetrics(duration time.Duration, status, kind string) {
	meter := telemetry.MeterWithSuffix("inference/route")
	operation := attribute.String(telemetry.AttrInferenceOperation, c.operation)

	if counter, err := meter.Int64Counter("executions.total",
		metric.WithDescription("Routed requests by outcome")); err == nil {
		counter.Add(c.ctx, 1, metric.WithAttributes(
			operation, attribute.String("status", status)))
	}
	if histogram, err := meter.Float64Histogram("duration.seconds",
		metric.WithDescription("Routing duration, including failover waits")); err == nil {
		histogram.Record(c.ctx, duration.Seconds(), metric.WithAttributes(operation))
	}
	if c.fallbacks > 0 {
		if counter, err := meter.Int64Counter("fallbacks.total",
			metric.WithDescription("Routed requests that moved to another target")); err == nil {
			counter.Add(c.ctx, 1, metric.WithAttributes(operation))
		}
	}
	if c.retries > 0 {
		if counter, err := meter.Int64Counter("retries.total",
			metric.WithDescription("Same-target retry attempts")); err == nil {
			counter.Add(c.ctx, int64(c.retries), metric.WithAttributes(
				operation, attribute.String(telemetry.AttrErrorKind, c.lastKind)))
		}
	}
	if kind != "" {
		if counter, err := meter.Int64Counter("errors.total",
			metric.WithDescription("Routed requests that failed on every target")); err == nil {
			counter.Add(c.ctx, 1, metric.WithAttributes(
				operation, attribute.String(telemetry.AttrErrorKind, kind)))
		}
	}
}

// errorKind extracts the classified failure kind, or "unclassified".
func errorKind(err error) string {
	var classified *inference.Error
	if errors.As(err, &classified) && classified.Kind != "" {
		return string(classified.Kind)
	}
	return "unclassified"
}
