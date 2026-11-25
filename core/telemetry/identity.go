package telemetry

import (
	"context"

	"github.com/GizClaw/flowcraft/core/identity"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Attrs renders the run identity carried by ctx as span and log attributes.
//
// Rendering lives here rather than in core/identity so the identity package stays a
// value with no dependencies: a layer that only needs to carry a session id must not
// link the OpenTelemetry SDK. Every producer that opens a span renders it once — through
// [AttrsFor] when it already holds the span — and gets the same keys in the same order,
// because the order is the one identity.Run.Pairs defines.
//
// The identity is read-only here: this renders what the three injection points put
// on the context, and nothing in this package can change it. An empty context yields
// nothing rather than empty strings — a span outside a session is a legitimate span,
// and an empty attribute is a filter that matches everything.
func Attrs(ctx context.Context) []attribute.KeyValue {
	run, ok := identity.Lookup(ctx)
	if !ok {
		return nil
	}
	return AttrsOf(run)
}

// AttrsFor renders the same attributes as [Attrs], but only when span will keep them.
//
// A producer that has already opened its span calls this instead of Attrs. A span that is
// not recording — the turn's trace level filtered it, or no provider is installed — drops
// every attribute set on it, so rendering the identity first would allocate the whole
// attribute list for a value nobody stores. On a recording span this is exactly Attrs, so
// producers do not have to reason about which one they hold.
func AttrsFor(span trace.Span, ctx context.Context) []attribute.KeyValue {
	if !span.IsRecording() {
		return nil
	}
	return Attrs(ctx)
}

// AttrsOf renders one identity value, for a caller that holds the value instead of
// a context (a test, or a layer assembling attributes for a record).
func AttrsOf(run identity.Run) []attribute.KeyValue {
	pairs := run.Pairs()
	if len(pairs) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, attribute.String(pair[0], pair[1]))
	}
	return out
}
