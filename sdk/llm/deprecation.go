package llm

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// deprecatedModelWarned dedupes the per-(provider, model) deprecation
// warning emitted by the resolver. Using a sync.Map keeps the
// fast-path lock-free; the false-sharing window between probe and
// store is harmless (worst case: a duplicate warning the very first
// time two goroutines race on the same model — one warning per
// process is the goal, not strict atomicity).
var deprecatedModelWarned sync.Map // key = provider + "/" + model

// warnDeprecatedModel emits at most one telemetry warning per
// (provider, model) over the lifetime of the process. Called from
// the resolver after a successful lookup whose ModelInfo.Deprecation
// is non-zero.
//
// The warning carries the announced retirement date and recommended
// replacement (when available) as structured fields so dashboards
// can group / alert on impending shutdowns. ctx is the resolve-call
// context — it is honoured for tracing correlation but the warning
// itself is best-effort and never blocks.
func warnDeprecatedModel(ctx context.Context, provider, model string, d ModelDeprecation) {
	key := provider + "/" + model
	if _, loaded := deprecatedModelWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}

	fields := []otellog.KeyValue{
		otellog.String("provider", provider),
		otellog.String("model", model),
	}
	if !d.RetiresAt.IsZero() {
		// Day-precision string so the warning reads naturally in
		// log searches; the structured field stays machine-parseable.
		fields = append(fields, otellog.String("retires_at", d.RetiresAt.UTC().Format("2006-01-02")))
	}
	if d.Replacement != "" {
		fields = append(fields, otellog.String("replacement", d.Replacement))
	}
	if d.Notes != "" {
		fields = append(fields, otellog.String("notes", d.Notes))
	}
	telemetry.Warn(ctx, "llm: resolving deprecated model — plan migration", fields...)
}

// resetDeprecatedModelWarnedForTest clears the dedup state so tests
// can exercise the "warns first time" branch repeatedly. Not exported
// in the production API surface.
func resetDeprecatedModelWarnedForTest() {
	deprecatedModelWarned.Range(func(k, _ any) bool {
		deprecatedModelWarned.Delete(k)
		return true
	})
}
