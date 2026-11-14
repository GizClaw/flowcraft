package delegation

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traceParent renders ctx's span context the way a W3C peer spells it, or ""
// when the caller has no span.
//
// The job stores the string rather than the context. A job outlives the call that
// created it — that is the whole point of mode: async — and a context carries
// deadlines and values that must not leak into work that started later. The
// string is only the parent's identity.
func traceParent(ctx context.Context) string {
	if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// withParentTrace reattaches the span context a job recorded, so the child's run
// joins the trace it came from instead of starting a trace of its own.
//
// The parent is attached as a remote parent: the child is a continuation of a
// trace that crossed a boundary — a job queue can be one — and the span it
// descends from has already ended by the time the child starts. An empty or
// unparseable value leaves the context alone: a job created without a span is a
// job whose child starts its own trace, not an error.
func withParentTrace(ctx context.Context, parent string) context.Context {
	if parent == "" {
		return ctx
	}
	extracted := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier{"traceparent": parent})
	span := trace.SpanContextFromContext(extracted)
	if !span.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, span)
}
