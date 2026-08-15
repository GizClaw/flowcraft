package runtime

import (
	"context"
	"log"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
)

// agentLifecyclePrefix is the subject root for runtime agent lifecycle
// events. It deliberately sits outside the engine-owned "agent.run.*"
// namespace.
const agentLifecyclePrefix = "runtime.agent."

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
	payload AgentLifecycleEvent,
) {
	if r == nil || r.bus == nil || isNilContext(ctx) {
		return
	}
	envelope, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		log.Printf("runtime: lifecycle event %q: %v", subject, err)
		return
	}
	if err := r.bus.Publish(ctx, envelope); err != nil {
		log.Printf("runtime: lifecycle event %q: %v", subject, err)
	}
}
