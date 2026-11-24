package telemetry

import (
	"context"

	"github.com/GizClaw/flowcraft/core/identity"

	"go.opentelemetry.io/otel/attribute"
)

// Attrs renders the run identity carried by ctx as span and log attributes.
//
// Rendering lives here rather than in core/identity so the identity package stays a
// value with no dependencies: a layer that only needs to carry a session id must not
// link the OpenTelemetry SDK. Every producer that opens a span calls Attrs once and
// gets the same keys in the same order, because the order is the one
// identity.Run.Pairs defines.
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
