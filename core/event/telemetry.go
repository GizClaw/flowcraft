package event

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Bus instrumentation. A dropped envelope is the one bus outcome nothing
// else reports by default: a bus nobody attaches an Observer to keeps
// running noopObserver, the drop tally only moves [MemoryBus.Dropped],
// and nothing in core polls that. So the bus reports its own drops —
// one counter increment per publish that dropped, plus a rate-limited
// log line — whether or not an application observer is attached.
//
// Subjects carry run ids and are unsuitable as metric attributes, so the
// counter is labelled by drop reason only.

// dropLogInterval bounds how often one bus logs a drop summary. Drops
// happen at publish rate, so an unthrottled line would flood the log.
const dropLogInterval = 30 * time.Second

var (
	eventMeter = telemetry.MeterWithSuffix("event")

	eventDrops, _ = eventMeter.Int64Counter(
		"drops.total",
		metric.WithDescription("Envelopes dropped by reason"))
)

// recordDrops counts one publish's drops and logs a rate-limited
// summary. It runs outside every bus lock (see MemoryBus.fireObserver).
func (b *MemoryBus) recordDrops(ctx context.Context, env Envelope, drops []dropCb) {
	if len(drops) == 0 {
		return
	}
	counts := make(map[DropReason]int, len(drops))
	for _, drop := range drops {
		counts[drop.reason]++
	}
	for reason, n := range counts {
		eventDrops.Add(ctx, int64(n), metric.WithAttributes(
			attribute.String("event.drop_reason", reason.String())))
	}

	// Losing an envelope the bus was asked to deliver is reported at
	// Warn; a voluntary Sample-policy rejection is not a loss, so it
	// stays on the debug path with the rest of the drop detail.
	loss := false
	for reason := range counts {
		if reason != DropReasonSampled {
			loss = true
			break
		}
	}
	if !loss && !telemetry.DebugEnabled(ctx) {
		return
	}
	if !b.claimDropLog(time.Now()) {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("event.subject", string(env.Subject)),
		attribute.Int("event.drops", len(drops)),
		attribute.String("event.drop_reasons", summarizeDropReasons(drops)),
		attribute.Int64("event.dropped_total", b.dropped.Load()),
	}
	if loss {
		telemetry.Warn(ctx, "event memory: envelopes dropped", attrs...)
		return
	}
	telemetry.Debug(ctx, "event memory: envelopes dropped", attrs...)
}

// claimDropLog reports whether this drop may be logged: at most one
// summary per bus per interval, claimed with a compare-and-swap so
// concurrent publishers do not all decide to log. The stamp keeps its
// monotonic reading, so a wall-clock step cannot silence the log.
func (b *MemoryBus) claimDropLog(now time.Time) bool {
	last := b.dropLogAt.Load()
	if last != nil && now.Sub(*last) < dropLogInterval {
		return false
	}
	return b.dropLogAt.CompareAndSwap(last, &now)
}
