package agent

import (
	"context"
	"math"
	"time"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/outcome"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instruments are named with an explicit agent. prefix rather than relying on the
// meter scope: an exporter is free to drop the scope, and "errors.total" means different
// things in the tool, route and agent layers.
//
// This file emits the turn's series. Instruments are resolved per turn for the same
// reason the tool and route layers do it: the global meter provider is swapped in tests,
// and the SDK caches meters and instruments behind the lookup.
//
// What is a metric and what is a span attribute is a division of labour: a span answers
// "why was this turn slow", a metric answers "is the fleet degrading". So the labels here
// are the low-cardinality ones — agent, state, kind, model — and never a session id, a
// task id or a tool call id. Those belong on the span, where they identify one record
// rather than splitting a series in two.

// Status labels for the turn counter. They mirror the terminal task states that matter
// to an operator rather than every state the vocabulary has.
const (
	statusCompleted = "completed"
	statusFailed    = "failed"
	statusCanceled  = "canceled"
	statusInputWait = "input-required"
)

// recordTurnMetrics emits one turn's counter and duration.
func recordTurnMetrics(ctx context.Context, req Request, outcome TurnOutcome) {
	meter := telemetry.MeterWithSuffix("agent")
	state := turnStatus(outcome)

	if counter, err := meter.Int64Counter("agent.turns",
		metric.WithDescription("Turns by terminal state")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(identity.AttrAgentName, req.Agent),
			attribute.String(telemetry.AttrAgentState, state)))
	}
	if outcome.Err != nil {
		kind, _ := FailureKindFrom(ctx)
		if counter, err := meter.Int64Counter("agent.turn.errors",
			metric.WithDescription("Failed turns by classification")); err == nil {
			counter.Add(ctx, 1, metric.WithAttributes(
				attribute.String(identity.AttrAgentName, req.Agent),
				attribute.String(telemetry.AttrErrorKind, string(kind))))
		}
	}
}

// recordTurnDuration emits how long the turn took. It is separate from the counter
// because a duration is only meaningful for a turn that finished — which is the caller's
// condition, not this one: only finish() records, so a zero here is a turn the clock could
// not resolve. Windows quantizes time.Since to the timer tick, which makes an instant turn
// exactly zero, and dropping that observation made the series platform-dependent.
func recordTurnDuration(ctx context.Context, req Request, elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	meter := telemetry.MeterWithSuffix("agent")
	if histogram, err := meter.Float64Histogram("agent.turn.duration",
		metric.WithDescription("Turn duration, from the first event to the terminal one")); err == nil {
		histogram.Record(ctx, elapsed.Seconds(),
			metric.WithAttributes(attribute.String(identity.AttrAgentName, req.Agent)))
	}
}

// recordTurnUsage emits the turn's token and cost totals.
//
// They are counters rather than span attributes because a dashboard sums them: putting
// tokens on a span makes the number readable per turn and unusable in aggregate.
func recordTurnUsage(ctx context.Context, req Request, outcome TurnOutcome) {
	if outcome.Usage.InputTokens == 0 && outcome.Usage.OutputTokens == 0 && outcome.Usage.Billing == nil {
		return
	}
	meter := telemetry.MeterWithSuffix("agent")
	agentAttr := attribute.String(identity.AttrAgentName, req.Agent)

	if counter, err := meter.Int64Counter("agent.tokens",
		metric.WithDescription("Tokens consumed by turns")); err == nil {
		if outcome.Usage.InputTokens > 0 {
			counter.Add(ctx, outcome.Usage.InputTokens, metric.WithAttributes(
				agentAttr, attribute.String("kind", "input")))
		}
		if outcome.Usage.OutputTokens > 0 {
			counter.Add(ctx, outcome.Usage.OutputTokens, metric.WithAttributes(
				agentAttr, attribute.String("kind", "output")))
		}
		if cached := outcome.Usage.Input.CacheReadTokens; cached != nil && *cached > 0 {
			counter.Add(ctx, *cached, metric.WithAttributes(
				agentAttr, attribute.String("kind", "cache_read")))
		}
	}
	if outcome.Usage.Billing != nil && outcome.Usage.Billing.Cost != nil {
		if counter, err := meter.Float64Counter("agent.cost",
			metric.WithDescription("Model spend reported by providers")); err == nil {
			counter.Add(ctx, moneyValue(*outcome.Usage.Billing.Cost), metric.WithAttributes(agentAttr))
		}
	}
}

// moneyValue renders a money amount as a float for the cost counter. A cost counter is
// the one place a float is acceptable: it is an aggregate for a dashboard, and the exact
// integer amount stays on the run record.
func moneyValue(m outcome.Money) float64 {
	return float64(m.Units) / math.Pow10(int(m.Scale))
}

// turnStatus maps the terminal state onto the counter's label, defaulting to "failed":
// a turn that reported no state is a turn nobody can resume, which is a failure.
func turnStatus(outcome TurnOutcome) string {
	switch {
	case outcome.State == StateCompleted:
		return statusCompleted
	case outcome.State == StateInputRequired:
		return statusInputWait
	case outcome.State == StateCanceled:
		return statusCanceled
	case outcome.Err != nil:
		return statusFailed
	case outcome.State == StateFailed || outcome.State == StateRejected || outcome.State == "":
		return statusFailed
	default:
		return string(outcome.State)
	}
}
