package ltm

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	assemblerMeter = telemetry.MeterWithSuffix("memory.assembler")

	assemblerCacheHits, _ = assemblerMeter.Int64Counter(
		"cache.hits.total",
		metric.WithDescription("ContextAssembler cache hits by layer"),
	)
	assemblerCacheMisses, _ = assemblerMeter.Int64Counter(
		"cache.misses.total",
		metric.WithDescription("ContextAssembler cache misses by layer"),
	)
)

func recordAssemblerCacheHit(ctx context.Context, layer string) {
	assemblerCacheHits.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", layer)))
}

func recordAssemblerCacheMiss(ctx context.Context, layer string) {
	assemblerCacheMisses.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", layer)))
}
