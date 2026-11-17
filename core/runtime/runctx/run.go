// Package runctx is the deprecated home of the run identity.
//
// This shim keeps an outside consumer compiling for one minor release. It
// forwards every symbol and adds nothing.
//
// Deprecated: use [github.com/GizClaw/flowcraft/core/correlation]. The carrier
// moved to the contracts tier because tool, sandbox, inference and agent read
// the identity without importing the assembly: a package under core/runtime made
// every one of those layers depend on the assembly for a context value. The
// attribute names moved with it, so "who the work belongs to" has one home.
package runctx

import (
	"context"

	"github.com/GizClaw/flowcraft/core/correlation"

	"go.opentelemetry.io/otel/attribute"
)

// Run is an alias of [correlation.Run].
//
// Deprecated: use [correlation.Run].
type Run = correlation.Run

// WithRun forwards to [correlation.WithRun].
//
// Deprecated: use [correlation.WithRun].
func WithRun(ctx context.Context, run Run) context.Context {
	return correlation.WithRun(ctx, run)
}

// From forwards to [correlation.From].
//
// Deprecated: use [correlation.From].
func From(ctx context.Context) (Run, bool) {
	return correlation.From(ctx)
}

// Attrs forwards to [correlation.Attrs].
//
// Deprecated: use [correlation.Attrs].
func Attrs(ctx context.Context) []attribute.KeyValue {
	return correlation.Attrs(ctx)
}
