package correlation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/correlation"
)

// The run identity is attached once per run and read once per span, so both ends
// are worth a number: a deployment that attaches it in several context values
// pays for each, and a span that reads it pays for the rendering.
func BenchmarkWithRun(b *testing.B) {
	full := correlation.Run{
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
			_ = correlation.WithRun(ctx, full)
		}
	})

	b.Run("chained", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			// The shape a deployment used to build: tenant, then runtime, then task.
			step := correlation.WithRun(ctx, correlation.Run{SessionID: "conv-1", AgentName: "writer", TenantID: "acme"})
			step = correlation.WithRun(step, correlation.Run{RuntimeID: "rt-1", Manifest: "sha256:manifest"})
			_ = correlation.WithRun(step, correlation.Run{TaskID: "task-1", Turn: 3})
		}
	})

	b.Run("attrs", func(b *testing.B) {
		ctx := correlation.WithRun(context.Background(), full)
		b.ReportAllocs()
		for b.Loop() {
			_ = correlation.Attrs(ctx)
		}
	})
}
