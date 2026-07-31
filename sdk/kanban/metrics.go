package kanban

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

// metrics are the board's own instruments. They cover card flow only.
// Execution metrics — how long an agent ran, how many are busy — belong
// to the executor: the board never runs anything and so cannot time
// anything.
type metrics struct {
	submitted metric.Int64Counter
	terminal  metric.Int64Counter
	latency   metric.Float64Histogram
}

func newMetrics(ctx context.Context) *metrics {
	m := telemetry.Meter()
	out := &metrics{}
	var err error

	out.submitted, err = m.Int64Counter("kanban.cards.submitted.total",
		metric.WithDescription("Cards admitted to the board"))
	warnOnErr(ctx, err, "kanban.cards.submitted.total")

	out.terminal, err = m.Int64Counter("kanban.cards.terminal.total",
		metric.WithDescription("Cards reaching a terminal state, by status"))
	warnOnErr(ctx, err, "kanban.cards.terminal.total")

	out.latency, err = m.Float64Histogram("kanban.cards.latency.seconds",
		metric.WithDescription("Time from submission to terminal state"),
		metric.WithUnit("s"))
	warnOnErr(ctx, err, "kanban.cards.latency.seconds")

	return out
}

func warnOnErr(ctx context.Context, err error, name string) {
	if err == nil {
		return
	}
	telemetry.Warn(ctx, "kanban: failed to create metric",
		otellog.String("metric", name),
		otellog.String(telemetry.AttrErrorMessage, err.Error()))
}

func (m *metrics) cardSubmitted(ctx context.Context, targetAgentID, producer string) {
	if m == nil || m.submitted == nil {
		return
	}
	m.submitted.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.AttrKanbanTargetAgentID, targetAgentID),
		attribute.String(telemetry.AttrKanbanProducerID, producer)))
}

// cardTransitioned records terminal arrivals. Intermediate transitions
// are observable on the bus; counting them as metrics would add
// cardinality without answering a question a dashboard asks.
func (m *metrics) cardTransitioned(ctx context.Context, snap *Card) {
	if m == nil || snap == nil || !snap.Status.IsTerminal() {
		return
	}
	target := ""
	if snap.Task != nil {
		target = snap.Task.TargetAgentID
	}
	attrs := metric.WithAttributes(
		attribute.String(telemetry.AttrKanbanTargetAgentID, target),
		attribute.String(telemetry.AttrKanbanProducerID, snap.Producer),
		attribute.String("status", string(snap.Status)),
	)
	if m.terminal != nil {
		m.terminal.Add(ctx, 1, attrs)
	}
	if m.latency != nil {
		m.latency.Record(ctx, snap.Elapsed().Seconds(), attrs)
	}
}
