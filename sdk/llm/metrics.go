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
	// llmCachedInputTokens is the subset of llmInputTokens that hit
	// the provider's prompt cache (TokenUsage.CachedInputTokens).
	// Kept as an independent counter rather than a dimension on the
	// existing input counter so dashboards can compute hit-rate
	// without splitting an existing aggregate. See sdk/model.TokenUsage
	// for the per-provider wire-name normalisation rules.
	llmCachedInputTokens, _ = llmMeter.Int64Counter("tokens.input.cached.total", metric.WithDescription("Total LLM input tokens served from the provider's prompt cache (subset of tokens.input.total)"))
)

// UsageSpanAttrs returns the canonical OpenTelemetry span-attribute
// set for one TokenUsage, mirroring what RecordLLMMetrics emits to
// the metrics pipeline. Producers (sdkx adapters today, anything
// driving llm.LLM tomorrow) MUST use this helper instead of
// hand-rolling attribute.Int64 calls so the trace and metrics
// surfaces stay in lock-step — adding a new TokenUsage field becomes
// a single-touch change.
//
// Semantics:
//
//   - InputTokens / OutputTokens are emitted unconditionally so a
//     successful call always carries the basic usage shape, even
//     when both are zero (some providers return zero on error
//     fall-through paths and dashboards still want a row).
//   - CachedInputTokens is emitted ONLY when > 0 to match the
//     model.TokenUsage `omitempty` wire convention; providers that
//     do not surface a cache breakdown therefore add no per-call
//     span weight.
func UsageSpanAttrs(usage TokenUsage) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens),
	}
	if usage.CachedInputTokens > 0 {
		attrs = append(attrs, attribute.Int64(telemetry.AttrLLMCachedInputTokens, usage.CachedInputTokens))
	}
	return attrs
}

// RecordLLMMetrics records standard LLM call metrics.
//
// usage.CachedInputTokens, when > 0, is also accumulated into the
// dedicated tokens.input.cached.total counter. It is always a subset
// of usage.InputTokens (enforced upstream by the adapter normalisation
// in sdkx/llm) so a dashboard panel for cache hit-rate can divide
// the two counters directly without per-row sanity checks.
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
		// Emit the cache counter only when the adapter actually
		// reported a cache hit — providers that do not surface a
		// breakdown leave the field zero, and an unconditional Add
		// would inflate the "no breakdown reported" case to a real
		// 0-bucket data point.
		if usage.CachedInputTokens > 0 {
			llmCachedInputTokens.Add(ctx, usage.CachedInputTokens, tokenAttrs)
		}
	}
}
