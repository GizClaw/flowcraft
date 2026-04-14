package llm

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	llmMeter = telemetry.MeterWithSuffix("llm")

	llmRequestCount, _ = llmMeter.Int64Counter("requests.total", metric.WithDescription("Total LLM requests"))
	llmDuration, _     = llmMeter.Float64Histogram("duration.seconds", metric.WithDescription("LLM call duration"))
	llmInputTokens, _  = llmMeter.Int64Counter("tokens.input.total", metric.WithDescription("Total LLM input tokens"))
	llmOutputTokens, _ = llmMeter.Int64Counter("tokens.output.total", metric.WithDescription("Total LLM output tokens"))
)

// RecordLLMMetrics records standard LLM call metrics.
func RecordLLMMetrics(ctx context.Context, provider, model, status string, duration time.Duration, usage TokenUsage) {
	attrs := metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("model", model),
		attribute.String("status", status),
	)
	llmRequestCount.Add(ctx, 1, attrs)
	llmDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("model", model),
	))
	if usage.InputTokens > 0 {
		tokenAttrs := metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
		)
		llmInputTokens.Add(ctx, usage.InputTokens, tokenAttrs)
		llmOutputTokens.Add(ctx, usage.OutputTokens, tokenAttrs)
	}
}
