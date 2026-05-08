package engine

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// tracingScope is the OTel instrumentation scope name for the spans
// emitted by [TracingMiddleware]. Kept distinct from per-engine
// scopes (e.g. "graph.execute") so an operator can enable/disable
// host-level tracing independently of the engine's own internal
// spans via the OTel SDK's view filtering.
const tracingScope = "github.com/GizClaw/flowcraft/sdk/engine"

// TracingMiddleware returns a [HostMiddleware] that wraps every
// Host method call in an OTel span. It is the default observability
// layer for engines that emit envelopes, checkpoint, or talk back
// to users — making the cross-process surface visible in any
// trace UI without per-engine instrumentation.
//
// Spans created (one per Host call):
//
//   - engine.host.publish      — attribs: messaging.destination (subject),
//     event.id, event.run_id (from envelope headers when present)
//   - engine.host.checkpoint   — attribs: checkpoint.run_id, checkpoint.node_id,
//     checkpoint.seq
//   - engine.host.ask_user     — attribs: prompt.kind
//   - engine.host.report_usage — attribs: usage.model, usage.input_tokens,
//     usage.output_tokens
//
// Errors set the span status to Error and record the error.
//
// Span lifetimes are tight (the wrapped call) so this middleware
// does NOT decorate Interrupts() — that returns a long-lived
// channel and a span around it would either be a single point-in-
// time event (uninteresting) or last for the entire run (better
// modeled by the engine's own outer span).
//
// Compose it with other middlewares using [ComposeHost]; place it
// near the OUTER end of the stack so its spans wrap the work done
// by inner middlewares (rate limiting, budget enforcement, etc.).
func TracingMiddleware() HostMiddleware {
	tracer := otel.Tracer(tracingScope)
	return func(inner Host) Host {
		if inner == nil {
			return nil
		}
		return tracingHost{inner: inner, tracer: tracer}
	}
}

// tracingHost is the concrete decorator returned by
// [TracingMiddleware]. It implements Host directly (rather than
// using [HostFuncs]) because every Host method is decorated; the
// HostFuncs delegation overhead would be wasted.
type tracingHost struct {
	inner  Host
	tracer trace.Tracer
}

func (h tracingHost) Publish(ctx context.Context, env event.Envelope) error {
	ctx, span := h.tracer.Start(ctx, "engine.host.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.destination", string(env.Subject)),
			attribute.String("event.id", env.ID),
		),
	)
	defer span.End()
	if rid, ok := env.Headers[event.HeaderRunID]; ok && rid != "" {
		span.SetAttributes(attribute.String("event.run_id", rid))
	}
	if nid, ok := env.Headers[event.HeaderNodeID]; ok && nid != "" {
		span.SetAttributes(attribute.String("event.node_id", nid))
	}
	if err := h.inner.Publish(ctx, env); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// Interrupts is NOT decorated — see TracingMiddleware doc comment
// for the reasoning. Pass-through preserves the engine's existing
// select semantics on the channel.
func (h tracingHost) Interrupts() <-chan Interrupt {
	return h.inner.Interrupts()
}

func (h tracingHost) AskUser(ctx context.Context, prompt UserPrompt) (UserReply, error) {
	ctx, span := h.tracer.Start(ctx, "engine.host.ask_user",
		trace.WithAttributes(
			attribute.String("prompt.source", prompt.Source),
			attribute.Int("prompt.parts", len(prompt.Parts)),
		),
	)
	defer span.End()
	reply, err := h.inner.AskUser(ctx, prompt)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return reply, err
}

func (h tracingHost) Checkpoint(ctx context.Context, cp Checkpoint) error {
	ctx, span := h.tracer.Start(ctx, "engine.host.checkpoint",
		trace.WithAttributes(
			attribute.String("checkpoint.exec_id", cp.ExecID),
			attribute.String("checkpoint.step", cp.Step),
			attribute.Int("checkpoint.iteration", cp.Iteration),
		),
	)
	defer span.End()
	if err := h.inner.Checkpoint(ctx, cp); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func (h tracingHost) ReportUsage(ctx context.Context, usage model.TokenUsage) error {
	ctx, span := h.tracer.Start(ctx, "engine.host.report_usage",
		trace.WithAttributes(
			attribute.String("usage.model", usage.Model),
			attribute.Int64("usage.input_tokens", usage.InputTokens),
			attribute.Int64("usage.output_tokens", usage.OutputTokens),
		),
	)
	defer span.End()
	if err := h.inner.ReportUsage(ctx, usage); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}
