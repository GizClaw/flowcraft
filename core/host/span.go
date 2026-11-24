package host

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The two spans this layer owns. Each names a boundary only this package can see: opening a session
// builds a boundary the process cannot describe, and running a turn is the clock its children hang
// from.
const (
	spanSessionOpen = "host.session.open"
	spanRun         = "host.run"
)

// startSessionSpan opens the span for one conversation's boundary.
//
// The identity is attached first, so the span carries it like every span below it — the backend's
// exec spans and the tool calls that run inside them are children of a trace that already says which
// session it belongs to.
func startSessionSpan(ctx context.Context, session *Session) (context.Context, trace.Span) {
	ctx = session.identity(ctx)
	ctx, span := telemetry.TracerWithSuffix("host").Start(ctx, spanSessionOpen,
		trace.WithAttributes(attribute.String(identity.AttrAgentName, session.agent)))
	span.SetAttributes(telemetry.Attrs(ctx)...)
	return ctx, span
}

// failSessionSpan records why a session could not be opened.
//
// The trace carries the classification, never the message: an error string has variable parts — paths,
// backend names, operator text from an approver — and belongs in the log.
func failSessionSpan(ctx context.Context, span trace.Span, err error) {
	kind := "session_open_failed"
	if denial, ok := asDenial(err); ok {
		kind = string(denial.Check)
	}
	span.SetAttributes(attribute.String(telemetry.AttrErrorKind, kind))
	span.SetStatus(codes.Error, kind)
	telemetry.WarnErr(ctx, "host: opening a session failed", err)
}

// startRunSpan opens the span for one turn, which is the parent every model and tool span of that turn
// hangs from.
func startRunSpan(ctx context.Context, session *Session) (context.Context, trace.Span) {
	ctx, span := telemetry.TracerWithSuffix("host").Start(ctx, spanRun,
		trace.WithAttributes(
			attribute.String(identity.AttrAgentName, session.agent),
			attribute.String(telemetry.AttrAgentTraceLevel, session.deploy.observability.Trace.String()),
		))
	span.SetAttributes(telemetry.Attrs(ctx)...)
	return ctx, span
}

// finishRunSpan records the turn's terminal state and closes it.
func finishRunSpan(ctx context.Context, span trace.Span, state agent.TaskState, failures int) {
	if state != "" {
		span.SetAttributes(attribute.String(telemetry.AttrAgentState, string(state)))
	}
	kind, ok := agent.FailureKindFrom(ctx)
	if failures == 0 && !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if !ok {
		kind = agent.FailureUnclassified
	}
	span.SetAttributes(attribute.String(telemetry.AttrErrorKind, string(kind)))
	span.SetStatus(codes.Error, string(kind))
	telemetry.Warn(ctx, "host: the turn failed",
		attribute.String(telemetry.AttrErrorKind, string(kind)))
}
