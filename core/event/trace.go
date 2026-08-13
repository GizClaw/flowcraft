package event

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// SpanContext returns the OTel SpanContext encoded in this envelope's
// TraceID / SpanID fields, or an invalid SpanContext if the envelope
// was produced outside of a span (or the IDs are malformed).
//
// The returned SpanContext has its Remote flag set so downstream code
// that uses [trace.ContextWithRemoteSpanContext] / [WithRemoteContext]
// produces the correct "follows-from-remote-process" semantics in the
// trace UI.
//
// Use [Envelope.WithRemoteContext] when you want to attach the
// envelope's parent to a context for child span creation.
func (e Envelope) SpanContext() trace.SpanContext {
	if e.TraceID == "" || e.SpanID == "" {
		return trace.SpanContext{}
	}
	tid, err := trace.TraceIDFromHex(e.TraceID)
	if err != nil {
		return trace.SpanContext{}
	}
	sid, err := trace.SpanIDFromHex(e.SpanID)
	if err != nil {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  sid,
		// TraceFlags: sampled by default — envelopes are only
		// produced for sampled traces upstream (the producer's
		// SpanFromContext check in NewEnvelope), so passing
		// FlagsSampled here aligns the consumer's downstream
		// child spans with the upstream sampling decision.
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

// WithRemoteContext returns a child context whose parent span is the
// remote span carried by this envelope. Use it on the SUBSCRIBER side
// when starting a span that should appear as a child of the
// envelope-emitting span:
//
//	ctx := env.WithRemoteContext(ctx)
//	ctx, span := tracer.Start(ctx, "consumer.handle")
//	defer span.End()
//
// If the envelope carries no usable parent (no TraceID, malformed
// IDs), the original context is returned unchanged so callers do not
// need a separate code path for "no parent" envelopes.
func (e Envelope) WithRemoteContext(ctx context.Context) context.Context {
	sc := e.SpanContext()
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
