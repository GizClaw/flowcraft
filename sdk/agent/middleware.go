package agent

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// HostMiddleware decorates a Host with policy / observability /
// resource-management behaviour. The pod / agent layer typically
// stacks several middlewares (audit → rate-limit → budget →
// secret-resolve) around a base Host built from a PodSpec; this
// type and the [ComposeHost] helper exist so the stack can be
// declared as a slice instead of N levels of struct embedding.
//
// Convention:
//
//   - Middleware ordering matches the slice order: ComposeHost(base,
//     A, B, C) returns C(B(A(base))). The first middleware in the
//     slice is the OUTERMOST wrapper and therefore runs first when an
//     engine calls a Host method. This matches how HTTP middleware
//     stacks are normally declared.
//   - Each middleware MUST return a Host value that delegates
//     unchanged for any sub-interface it does not specifically
//     decorate. The [HostFuncs] adapter is provided to make this
//     easy: zero-value func fields delegate to the wrapped Host.
//   - Middlewares are invoked from any goroutine — implementations
//     must be safe for concurrent use, mirroring the Host contract.
type HostMiddleware func(Host) Host

// ComposeHost returns base wrapped by every middleware in mws, in
// declaration order (first = outermost). Returns base unchanged when
// mws is empty.
func ComposeHost(base Host, mws ...HostMiddleware) Host {
	// Apply in reverse so the first slice entry ends up as the
	// outermost wrapper. ComposeHost(base, A, B) ≡ A(B(base)) so a
	// caller reading the slice top-down sees "A first, then B".
	h := base
	for _, mw := range slices.Backward(mws) {
		if mw == nil {
			continue
		}
		h = mw(h)
		if h == nil {
			// A middleware that returns nil would silently break the
			// chain at the next call. Refuse the whole compose so the
			// programming bug surfaces immediately at assembly time.
			panic("agent.ComposeHost: middleware returned nil Host")
		}
	}
	return h
}

// HostFuncs is the func-field adapter that lets a middleware decorate
// just the Host methods it cares about while delegating the rest to
// an underlying Host. Construct one with the inner host as Inner and
// override only the func fields you need:
//
//	wrapped := agent.HostFuncs{
//	    Inner: base,
//	    ReportUsageFn: func(ctx context.Context, u inference.Usage) error {
//	        // budget enforcement here
//	        return base.ReportUsage(ctx, u)
//	    },
//	}
//
// Every nil func field falls through to Inner so partial decorators
// stay short. Inner MUST be non-nil; a nil Inner is a programming
// bug and triggers a panic at the first delegated call.
type HostFuncs struct {
	Inner Host

	PublishFn     func(ctx context.Context, env event.Envelope) error
	InterruptsFn  func() <-chan Interrupt
	AskUserFn     func(ctx context.Context, prompt UserPrompt) (UserReply, error)
	CheckpointFn  func(ctx context.Context, cp Checkpoint) error
	ReportUsageFn func(ctx context.Context, usage inference.Usage) error
}

// Publish routes through PublishFn or Inner.
func (h HostFuncs) Publish(ctx context.Context, env event.Envelope) error {
	if h.PublishFn != nil {
		return h.PublishFn(ctx, env)
	}
	return h.requireInner().Publish(ctx, env)
}

// Interrupts routes through InterruptsFn or Inner.
func (h HostFuncs) Interrupts() <-chan Interrupt {
	if h.InterruptsFn != nil {
		return h.InterruptsFn()
	}
	return h.requireInner().Interrupts()
}

// AskUser routes through AskUserFn or Inner.
func (h HostFuncs) AskUser(ctx context.Context, prompt UserPrompt) (UserReply, error) {
	if h.AskUserFn != nil {
		return h.AskUserFn(ctx, prompt)
	}
	return h.requireInner().AskUser(ctx, prompt)
}

// Checkpoint routes through CheckpointFn or Inner.
func (h HostFuncs) Checkpoint(ctx context.Context, cp Checkpoint) error {
	if h.CheckpointFn != nil {
		return h.CheckpointFn(ctx, cp)
	}
	return h.requireInner().Checkpoint(ctx, cp)
}

// ReportUsage routes through ReportUsageFn or Inner.
func (h HostFuncs) ReportUsage(ctx context.Context, usage inference.Usage) error {
	if h.ReportUsageFn != nil {
		return h.ReportUsageFn(ctx, usage)
	}
	return h.requireInner().ReportUsage(ctx, usage)
}

// requireInner panics with a clear message when a delegated method
// is invoked without an Inner host configured. Caught at the first
// call rather than producing a confusing nil-pointer trace several
// frames in.
func (h HostFuncs) requireInner() Host {
	if h.Inner == nil {
		panic("agent.HostFuncs: Inner is nil; cannot delegate")
	}
	return h.Inner
}

// ---------- OTel tracing middleware ----------

// tracingScope is the OTel instrumentation scope name for the spans
// emitted by [TracingMiddleware]. Kept distinct from per-engine
// scopes (e.g. "graph.execute") so an operator can enable/disable
// host-level tracing independently of the engine's own internal
// spans via the OTel SDK's view filtering.
const tracingScope = "github.com/GizClaw/flowcraft/sdk/agent"

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
	ctx, span := h.tracer.Start(ctx, "agent.host.publish",
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
	ctx, span := h.tracer.Start(ctx, "agent.host.ask_user",
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
	ctx, span := h.tracer.Start(ctx, "agent.host.checkpoint",
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

func (h tracingHost) ReportUsage(ctx context.Context, usage inference.Usage) error {
	ctx, span := h.tracer.Start(ctx, "agent.host.report_usage",
		trace.WithAttributes(
			attribute.String("usage.provider", usage.Model.ID.Provider),
			attribute.String("usage.model", usage.Model.ID.Name),
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
