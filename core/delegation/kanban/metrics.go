package kanban

import (
	"context"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

const (
	AttrKanbanProducerID    = "kanban.producer.id"
	AttrKanbanTargetAgentID = "kanban.target.agent.id"
)

type metrics struct {
	submitted metric.Int64Counter
	terminal  metric.Int64Counter
	latency   metric.Float64Histogram
}

func newMetrics(ctx context.Context) *metrics {
	meter := telemetry.Meter()
	out := &metrics{}
	var err error
	out.submitted, err = meter.Int64Counter(
		"delegation.kanban.cards.submitted.total",
		metric.WithDescription("Delegations admitted to the kanban backend"))
	warnOnMetricError(ctx, err, "delegation.kanban.cards.submitted.total")
	out.terminal, err = meter.Int64Counter(
		"delegation.kanban.cards.terminal.total",
		metric.WithDescription("Delegations reaching a terminal state"))
	warnOnMetricError(ctx, err, "delegation.kanban.cards.terminal.total")
	out.latency, err = meter.Float64Histogram(
		"delegation.kanban.cards.latency.seconds",
		metric.WithDescription("Time from delegation admission to terminal state"),
		metric.WithUnit("s"))
	warnOnMetricError(ctx, err, "delegation.kanban.cards.latency.seconds")
	return out
}

func warnOnMetricError(ctx context.Context, err error, name string) {
	if err == nil {
		return
	}
	telemetry.Warn(ctx, "delegation kanban: failed to create metric",
		otellog.String("metric", name),
		otellog.String(telemetry.AttrErrorMessage, err.Error()))
}

func (m *metrics) cardSubmitted(ctx context.Context, card *Card) {
	if m == nil || m.submitted == nil || card == nil || card.Task == nil {
		return
	}
	m.submitted.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKanbanProducerID, card.Producer),
		attribute.String(AttrKanbanTargetAgentID, card.Task.Request.Request.Target)))
}

func (m *metrics) cardTransitioned(ctx context.Context, card *Card) {
	if m == nil || card == nil || !card.Status.IsTerminal() {
		return
	}
	target := ""
	if card.Task != nil {
		target = card.Task.Request.Request.Target
	}
	attributes := metric.WithAttributes(
		attribute.String(AttrKanbanProducerID, card.Producer),
		attribute.String(AttrKanbanTargetAgentID, target),
		attribute.String("status", string(card.Status)))
	if m.terminal != nil {
		m.terminal.Add(ctx, 1, attributes)
	}
	if m.latency != nil {
		m.latency.Record(ctx, card.Elapsed().Seconds(), attributes)
	}
}
