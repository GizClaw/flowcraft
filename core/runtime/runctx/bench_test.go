package runctx_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

// The run identity is attached once per run and read once per span, so both ends
// are worth a number: a deployment that attaches it in several context values
// pays for each, and a span that reads it pays for the rendering.
func BenchmarkWithRun(b *testing.B) {
	full := runctx.Run{
		RuntimeID: "rt-1",
		TenantID:  "acme",
		SessionID: "conv-1",
		AgentName: "writer",
		Manifest:  "sha256:manifest",
	}

	b.Run("cold", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			_ = runctx.WithRun(ctx, full)
		}
	})

	b.Run("chained", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			// The shape a deployment used to build: tenant, then runtime, then task.
			step := runctx.WithRun(ctx, runctx.Run{SessionID: "conv-1", AgentName: "writer", TenantID: "acme"})
			step = runctx.WithRun(step, runctx.Run{RuntimeID: "rt-1", Manifest: "sha256:manifest"})
			_ = runctx.WithRun(step, runctx.Run{TaskID: "task-1", Turn: 3})
		}
	})

	b.Run("attrs", func(b *testing.B) {
		ctx := runctx.WithRun(context.Background(), full)
		b.ReportAllocs()
		for b.Loop() {
			_ = runctx.Attrs(ctx)
		}
	})
}
