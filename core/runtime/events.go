package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// agentLifecyclePrefix is the subject root for runtime agent lifecycle
// events. It deliberately sits outside the engine-owned "agent.run.*"
// namespace.
const agentLifecyclePrefix = "runtime.agent."

// runtimeRebuildPrefix is the subject root for runtime generation
// reload events. Like the agent lifecycle subjects, it sits outside the
// engine-owned "agent.run.*" namespace.
const runtimeRebuildPrefix = "runtime.rebuild."

// SubjectAgentRegistered returns the subject for a successful dynamic
// registration:
//
//	runtime.agent.<id>.registered
func SubjectAgentRegistered(id string) event.Subject {
	return event.Subject(agentLifecyclePrefix + agent.SanitiseID(id) + ".registered")
}

// SubjectAgentRemoved returns the subject for a successful dynamic
// removal:
//
//	runtime.agent.<id>.removed
func SubjectAgentRemoved(id string) event.Subject {
	return event.Subject(agentLifecyclePrefix + agent.SanitiseID(id) + ".removed")
}

// PatternAgentLifecycle matches every runtime agent lifecycle event.
func PatternAgentLifecycle() event.Pattern {
	return event.Pattern(agentLifecyclePrefix + ">")
}

// SubjectRuntimeRebuildStarted is published when a Reload begins
// building the next generation.
func SubjectRuntimeRebuildStarted() event.Subject {
	return event.Subject(runtimeRebuildPrefix + "started")
}

// SubjectRuntimeRebuildCompleted is published after a Reload atomically
// swapped the current generation.
func SubjectRuntimeRebuildCompleted() event.Subject {
	return event.Subject(runtimeRebuildPrefix + "completed")
}

// SubjectRuntimeRebuildFailed is published when a Reload aborts before
// any swap; the previous generation keeps serving.
func SubjectRuntimeRebuildFailed() event.Subject {
	return event.Subject(runtimeRebuildPrefix + "failed")
}

// PatternRuntimeRebuild matches every runtime generation reload event.
func PatternRuntimeRebuild() event.Pattern {
	return event.Pattern(runtimeRebuildPrefix + ">")
}

// RuntimeRebuildEvent is the payload of runtime.rebuild.* events.
type RuntimeRebuildEvent struct {
	GenerationID         uint64   `json:"generation_id"`
	PreviousGenerationID uint64   `json:"previous_generation_id,omitempty"`
	ReboundAgents        []string `json:"rebound_agents,omitempty"`
	DrainedAgents        []string `json:"drained_agents,omitempty"`
	Error                string   `json:"error,omitempty"`
}

// AgentLifecycleEvent is the payload of runtime.agent.* lifecycle
// events. It intentionally carries only identity and card summary, so
// the envelope stays small.
type AgentLifecycleEvent struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// publishLifecycleEvent is a best-effort notification: publish failures
// never change the outcome of register/remove and are only logged.
func (r *Runtime) publishLifecycleEvent(
	ctx context.Context,
	subject event.Subject,
	payload any,
) {
	if r == nil || r.bus == nil || isNilContext(ctx) {
		return
	}
	envelope, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		telemetry.WarnErr(ctx, "runtime: lifecycle event envelope failed", err,
			otellog.String("event.subject", string(subject)))
		return
	}
	if err := r.bus.Publish(ctx, envelope); err != nil {
		telemetry.WarnErr(ctx, "runtime: lifecycle event publish failed", err,
			otellog.String("event.subject", string(subject)))
	}
}
