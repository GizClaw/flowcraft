package host

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The session table's series. They are counters and an up/down counter rather than spans because they
// answer a different question: a span says what happened to one session, these say whether the table
// is drifting — sessions leaking, operators steering more than usual, eviction doing the work a limit
// should have.
//
// Labels are the low-cardinality ones: the agent, the steer intent, the outcome. A session id would
// turn every series into a histogram of one.
const (
	// steerAccepted is a steer that was queued for the running turn's next boundary.
	steerAccepted = "accepted"
	// steerIdle is a steer with no turn to apply it to.
	steerIdle = "idle"
	// steerUnsupported is a steer against a driver that cannot take one.
	steerUnsupported = "not_steerable"
	// steerFailed is a steer the driver refused.
	steerFailed = "refused"
)

// recordSessionOpened adds one to the open-session count.
func recordSessionOpened(ctx context.Context, agentName string) {
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64UpDownCounter("host.sessions",
		metric.WithDescription("Sessions currently open, by agent")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String(identity.AttrAgentName, agentName)))
	}
}

// recordSessionsClosed subtracts what a close or an eviction released.
func recordSessionsClosed(ctx context.Context, agentName string, count int) {
	if count <= 0 {
		return
	}
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64UpDownCounter("host.sessions",
		metric.WithDescription("Sessions currently open, by agent")); err == nil {
		counter.Add(ctx, int64(-count), metric.WithAttributes(attribute.String(identity.AttrAgentName, agentName)))
	}
}

// recordSteer counts one operator input by what happened to it.
//
// The refused outcomes are counted as well as the accepted ones: a deployment where steering keeps
// arriving while nothing is running is a deployment whose operators are racing the turn loop, and that
// is only visible from this side.
func recordSteer(ctx context.Context, in agent.SteerInput, outcome string) {
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64Counter("host.steers",
		metric.WithDescription("Operator inputs by intent and outcome")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("intent", string(in.Intent)),
			attribute.String("outcome", outcome)))
	}
}

// recordDenial counts one refusal by the rule that refused.
//
// policy emits nothing — a refusal is a value it returns — so the layer that acted on it is the one
// that records it. Counting by check keeps "this deployment is being refused for writes" separable
// from "for visibility".
func recordDenial(ctx context.Context, check policy.Check, agentName string) {
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64Counter("policy.denials",
		metric.WithDescription("Refusals by the rule that refused")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("check", string(check)),
			attribute.String(identity.AttrAgentName, agentName)))
	}
}

// recordEvicted counts the sessions a limit closed rather than a caller.
func recordEvicted(ctx context.Context, agentName string, count int) {
	if count <= 0 {
		return
	}
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64Counter("host.evicted",
		metric.WithDescription("Sessions closed by an idle limit")); err == nil {
		counter.Add(ctx, int64(count), metric.WithAttributes(attribute.String(identity.AttrAgentName, agentName)))
	}
}
