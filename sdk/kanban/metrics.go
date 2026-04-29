package kanban

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

// kanbanMetrics holds OTel instruments for the Kanban system.
type kanbanMetrics struct {
	tasksSubmitted metric.Int64Counter
	tasksDuration  metric.Float64Histogram
	agentsActive   metric.Int64UpDownCounter
}

func newKanbanMetrics(ctx context.Context) *kanbanMetrics {
	m := telemetry.Meter()
	km := &kanbanMetrics{}

	var err error

	km.tasksSubmitted, err = m.Int64Counter("kanban.tasks.submitted.total",
		metric.WithDescription("Total tasks submitted"))
	if err != nil {
		telemetry.Warn(ctx, "kanban: failed to create tasks.submitted metric", otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}

	km.tasksDuration, err = m.Float64Histogram("kanban.tasks.duration.seconds",
		metric.WithDescription("Task execution duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		telemetry.Warn(ctx, "kanban: failed to create tasks.duration metric", otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}

	km.agentsActive, err = m.Int64UpDownCounter("kanban.agents.active",
		metric.WithDescription("Number of currently active agents"))
	if err != nil {
		telemetry.Warn(ctx, "kanban: failed to create agents.active metric", otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}

	return km
}

func (km *kanbanMetrics) incTasksSubmitted(ctx context.Context, attrs ...attribute.KeyValue) {
	if km.tasksSubmitted != nil {
		km.tasksSubmitted.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func (km *kanbanMetrics) recordTaskDuration(ctx context.Context, seconds float64, attrs ...attribute.KeyValue) {
	if km.tasksDuration != nil {
		km.tasksDuration.Record(ctx, seconds, metric.WithAttributes(attrs...))
	}
}

func (km *kanbanMetrics) addAgentsActive(ctx context.Context, delta int64) {
	if km.agentsActive != nil {
		km.agentsActive.Add(ctx, delta)
	}
}
