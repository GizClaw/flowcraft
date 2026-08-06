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
	err error,
) {
	opAttr := attribute.String("inference.operation", string(operation))
	selected := routeTrace.Decision.Selected
	span.SetAttributes(
		attribute.String("route.selected.provider", selected.ID.Provider),
		attribute.String("route.selected.model", selected.ID.Name),
		attribute.Int("route.attempts", len(routeTrace.Attempts)),
		attribute.Int("route.fallbacks", len(routeTrace.Fallbacks)),
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
		span.AddEvent("route.attempt", trace.WithAttributes(attrs...))
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
	if len(routeTrace.Fallbacks) > 0 {
		routeFallbackCount.Add(ctx, 1, metric.WithAttributes(opAttr))
	}
	routeExecCount.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("status", "success")))
	span.SetStatus(codes.Ok, "OK")
	span.End()
}
