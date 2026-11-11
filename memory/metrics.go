package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/metric"
)

// assemblyMetrics carries the OTel instruments this assembly records. The
// instruments bind to the global meter provider, so they stay no-op until the
// host initializes telemetry.
type assemblyMetrics struct {
	contextRequests metric.Int64Counter
	contextItems    metric.Int64Counter
}

func newAssemblyMetrics() *assemblyMetrics {
	meter := telemetry.Meter()
	requests, _ := meter.Int64Counter("memory.context.requests",
		metric.WithDescription("Memory context requests served"))
	items, _ := meter.Int64Counter("memory.context.items",
		metric.WithDescription("Memory context items returned"))
	return &assemblyMetrics{contextRequests: requests, contextItems: items}
}

func (metrics *assemblyMetrics) contextServed(ctx context.Context, items int) {
	if metrics == nil {
		return
	}
	metrics.contextRequests.Add(ctx, 1)
	metrics.contextItems.Add(ctx, int64(items))
}
