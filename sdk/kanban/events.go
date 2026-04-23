package kanban

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// LegacyEventBus event types for Kanban operations. The catalogue below is the
// complete set of business actions an external observer can subscribe to via
// Board.Bus(); subscribing alone is sufficient to reconstruct every state
// transition the SDK exposes through Cards().
//
// When a new public action is added, it MUST add (1) a constant here, (2) a
// Publish call in the same critical section as the state change, and (3) a
// unit test under sdk/kanban/...
const (
	EventTaskSubmitted    = "kanban.task.submitted"
	EventTaskClaimed      = "kanban.task.claimed"
	EventTaskCompleted    = "kanban.task.completed"
	EventTaskFailed       = "kanban.task.failed"
	EventCallbackStart    = "kanban.callback.start"
	EventCallbackDone     = "kanban.callback.done"
	EventCronRuleCreated  = "kanban.cron.rule.created"
	EventCronRuleFired    = "kanban.cron.rule.fired"
	EventCronRuleDisabled = "kanban.cron.rule.disabled"
)

// payloadVersion is the schema version stamped onto every Kanban event payload.
// Future fields are additive; consumers that pin Version=1 keep working.
const payloadVersion = 1

// TaskSubmittedPayload is published when a task is submitted.
type TaskSubmittedPayload struct {
	Version       int            `json:"version"`
	CardID        string         `json:"card_id"`
	TargetAgentID string         `json:"target_agent_id"`
	Query         string         `json:"query"`
	RuntimeID     string         `json:"runtime_id"`
	Inputs        map[string]any `json:"inputs,omitempty"`
}

// TaskClaimedPayload is published when an agent claims a task.
type TaskClaimedPayload struct {
	Version       int    `json:"version"`
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
	Consumer      string `json:"consumer"`
}

// TaskCompletedPayload is published when a task is completed.
type TaskCompletedPayload struct {
	Version       int    `json:"version"`
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
	Output        string `json:"output"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

// TaskFailedPayload is published when a task fails.
type TaskFailedPayload struct {
	Version       int    `json:"version"`
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
	Error         string `json:"error"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

// CallbackStartPayload is published just before the callback message is sent
// to the Dispatcher Actor, so WS subscribers can start streaming.
type CallbackStartPayload struct {
	Version   int    `json:"version"`
	CardID    string `json:"card_id"`
	RuntimeID string `json:"runtime_id"`
	AgentID   string `json:"agent_id"`
	Query     string `json:"query"`
}

// CallbackDonePayload is published when the Dispatcher finishes processing
// a task callback, signaling the frontend to finalize the streamed message.
// Error is set when the callback turn ended unsuccessfully.
type CallbackDonePayload struct {
	Version   int    `json:"version"`
	CardID    string `json:"card_id"`
	RuntimeID string `json:"runtime_id"`
	AgentID   string `json:"agent_id"`
	Error     string `json:"error,omitempty"`
}

// CronRuleCreatedPayload is published when a new dynamic cron rule is registered.
type CronRuleCreatedPayload struct {
	Version    int    `json:"version"`
	ScheduleID string `json:"schedule_id"`
	AgentID    string `json:"agent_id"`
	Cron       string `json:"cron"`
	Query      string `json:"query"`
	Timezone   string `json:"timezone,omitempty"`
}

// CronRuleFiredPayload is published each time a cron rule fires and produces a task card.
type CronRuleFiredPayload struct {
	Version    int    `json:"version"`
	ScheduleID string `json:"schedule_id"`
	AgentID    string `json:"agent_id"`
	CardID     string `json:"card_id"`
	Query      string `json:"query"`
}

// CronRuleDisabledPayload is published when a cron rule is removed (e.g. via RemoveAgent).
type CronRuleDisabledPayload struct {
	Version    int    `json:"version"`
	ScheduleID string `json:"schedule_id"`
	AgentID    string `json:"agent_id"`
}

// eventEnvelope wraps a Kanban payload into an event.Event. It auto-stamps the
// payload's Version field via reflection-free type switch, so call sites stay
// terse.
func eventEnvelope(eventType string, payload any) event.Event {
	switch p := payload.(type) {
	case TaskSubmittedPayload:
		p.Version = payloadVersion
		payload = p
	case TaskClaimedPayload:
		p.Version = payloadVersion
		payload = p
	case TaskCompletedPayload:
		p.Version = payloadVersion
		payload = p
	case TaskFailedPayload:
		p.Version = payloadVersion
		payload = p
	case CronRuleCreatedPayload:
		p.Version = payloadVersion
		payload = p
	case CronRuleFiredPayload:
		p.Version = payloadVersion
		payload = p
	case CronRuleDisabledPayload:
		p.Version = payloadVersion
		payload = p
	}
	return event.Event{
		Type:      event.EventType(eventType),
		Timestamp: time.Now(),
		Payload:   payload,
	}
}
