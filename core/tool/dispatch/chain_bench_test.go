package dispatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/tool/middleware"
)

// BenchmarkExecuteWithChain measures the same call through the middleware a deployment
// usually declares, so the per-layer cost has a number.
func BenchmarkExecuteWithChain(b *testing.B) {
	executor, binding := benchExecutor(b,
		middleware.Recover(),
		middleware.Telemetry(),
		middleware.Timeout(5*time.Second, nil),
	)
	call := benchCall()
	b.ReportAllocs()
	for b.Loop() {
		if res := executor.Execute(context.Background(), binding, call); res.IsError {
			b.Fatalf("Execute: %s", res.Content.Text())
		}
	}
}
