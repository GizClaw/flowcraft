package route

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Routed calls get one route-level span per logical request. The span
// records the selector's decision, every attempt as a span event, and
// the final executed target — the "did we fall back, how often, why"
// story that per-attempt Runtime spans (each attempt delegates to a
// Runtime call, so they nest below this span) cannot tell on their own.

var (
	routeMeter = telemetry.MeterWithSuffix("inference.route")

	routeExecCount, _ = routeMeter.Int64Counter(
		"executions.total",
		metric.WithDescription("Total routed inference operations"))
	routeFallbackCount, _ = routeMeter.Int64Counter(
		"fallbacks.total",
		metric.WithDescription("Routed operations that fell back at least once"))
	routeRetryCount, _ = routeMeter.Int64Counter(
		"retries.total",
		metric.WithDescription("Same-target retry attempts"))
	routeCircuitOpens, _ = routeMeter.Int64Counter(
		"circuit.opens",
		metric.WithDescription("Circuit transitions to open"))
	routeCircuitSkips, _ = routeMeter.Int64Counter(
		"circuit.skips",
		metric.WithDescription("Attempts skipped because the circuit was open"))
	routeCircuitProbes, _ = routeMeter.Int64Counter(
		"circuit.half_open_probes",
		metric.WithDescription("Half-open circuit probes"))
)

func startRouteSpan(ctx context.Context, operation inference.Operation) (context.Context, trace.Span) {
	return telemetry.Tracer().Start(ctx, "inference.route."+string(operation),
		trace.WithAttributes(attribute.String("inference.operation", string(operation))))
}

// recordRoute closes a routed call: attempt events, route shape
// attributes, and final status. routeTrace is the value returned to
// the caller, so a failed routing still records the attempts that ran.
func recordRoute(
	ctx context.Context,
	span trace.Span,
	operation inference.Operation,
	routeTrace Trace,
	metadata inference.Metadata,
	err error,
) {
	opAttr := attribute.String("inference.operation", string(operation))
	selected := routeTrace.Decision.Selected
	retries := 0
	for _, attempt := range routeTrace.Attempts {
		if attempt.Trigger == AttemptTriggerRetry {
			retries++
		}
	}
	span.SetAttributes(
		attribute.String("route.selected.provider", selected.ID.Provider),
		attribute.String("route.selected.model", selected.ID.Name),
		attribute.Int("route.attempts", len(routeTrace.Attempts)),
		attribute.Int("route.fallbacks", len(routeTrace.Fallbacks)),
		attribute.Int("route.retries", retries),
	)
	for _, attempt := range routeTrace.Attempts {
		attrs := []attribute.KeyValue{
			attribute.String("target.provider", attempt.Target.ID.Provider),
			attribute.String("target.model", attempt.Target.ID.Name),
			attribute.String("phase", string(attempt.Phase)),
			attribute.String("trigger", string(attempt.Trigger)),
			attribute.String("outcome", string(attempt.Outcome)),
		}
		if attempt.ErrorKind != "" {
			attrs = append(attrs, attribute.String("error_kind", string(attempt.ErrorKind)))
		}
		if attempt.Number > 0 {
			attrs = append(attrs, attribute.Int("attempt", attempt.Number))
		}
		if attempt.BackoffMillis > 0 {
			attrs = append(attrs, attribute.Int64("backoff_ms", attempt.BackoffMillis))
		}
		if attempt.Circuit != "" {
			attrs = append(attrs, attribute.String("circuit", attempt.Circuit))
		}
		if attempt.CircuitTransition != "" {
			attrs = append(
				attrs,
				attribute.String("circuit_transition", attempt.CircuitTransition),
			)
		}
		if attempt.WireAttempts > 0 {
			attrs = append(attrs, attribute.Int("wire_attempts", attempt.WireAttempts))
		}
		span.AddEvent("route.attempt", trace.WithAttributes(attrs...))
		if attempt.Trigger == AttemptTriggerRetry {
			routeRetryCount.Add(ctx, 1, metric.WithAttributes(
				opAttr,
				attribute.String(telemetry.AttrLLMProvider, attempt.Target.ID.Provider),
				attribute.String(telemetry.AttrLLMModel, attempt.Target.ID.Name),
				attribute.String("error_kind", string(attempt.ErrorKind)),
			))
		}
	}
	if err != nil {
		routeExecCount.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("status", "error")))
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return
	}
	executed := routeTrace.Executed
	span.SetAttributes(
		attribute.String("route.executed.provider", executed.ID.Provider),
		attribute.String("route.executed.model", executed.ID.Name),
	)
	if metadata.RequestID != "" {
		span.SetAttributes(
			attribute.String(telemetry.AttrLLMRequestID, metadata.RequestID))
	}
	if metadata.ResponseID != "" {
		span.SetAttributes(
			attribute.String(telemetry.AttrLLMResponseID, metadata.ResponseID))
	}
	if len(routeTrace.Fallbacks) > 0 {
		routeFallbackCount.Add(ctx, 1, metric.WithAttributes(opAttr))
	}
	routeExecCount.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("status", "success")))
	span.SetStatus(codes.Ok, "OK")
	span.End()
}
