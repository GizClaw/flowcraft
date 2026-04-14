package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	pipelineMeter = telemetry.MeterWithSuffix("memory.pipeline")

	pipelineStageDurationMs, _ = pipelineMeter.Float64Histogram(
		"stage.duration.ms",
		metric.WithDescription("Fact pipeline stage duration in milliseconds"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 3000, 8000),
	)
	pipelineStageErrors, _ = pipelineMeter.Int64Counter(
		"stage.errors.total",
		metric.WithDescription("Fact pipeline stage failures"),
	)
	dedupFallbackTotal, _ = pipelineMeter.Int64Counter(
		"dedup.fallback.total",
		metric.WithDescription("Batch deduplication LLM failures leading to create-all fallback"),
	)
	conflictDecisionsTotal, _ = pipelineMeter.Int64Counter(
		"conflict.decisions.total",
		metric.WithDescription("Deduplication actions where the LLM set conflict_type (any action)"),
	)

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

func recordPipelineStage(ctx context.Context, stage string, ms float64, err error) {
	pipelineStageDurationMs.Record(ctx, ms, metric.WithAttributes(attribute.String("stage", stage)))
	if err != nil {
		pipelineStageErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", stage)))
	}
}

func recordDedupFallback(ctx context.Context) {
	dedupFallbackTotal.Add(ctx, 1)
}

func recordConflictDecision(ctx context.Context, ct ConflictType) {
	if ct == "" {
		return
	}
	conflictDecisionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("conflict_type", string(ct))))
}

func recordAssemblerCacheHit(ctx context.Context, layer string) {
	assemblerCacheHits.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", layer)))
}

func recordAssemblerCacheMiss(ctx context.Context, layer string) {
	assemblerCacheMisses.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", layer)))
}
