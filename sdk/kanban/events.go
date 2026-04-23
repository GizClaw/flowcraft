package kanban

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// EventKind labels the Kanban events surfaced via Board.Bus(). Subjects
// emitted on the wire follow the format documented in subjects.go (e.g.
// "kanban.card.<cardID>.task.submitted"); EventKind keeps the older
// flat-type vocabulary available for callers that want to reason about
// the action without parsing the Subject.
//
// EventKind values are also written to a "kind" header on every emitted
// Envelope so subscribers using a broad pattern (kanban.>) can route on
// kind without re-parsing the subject.
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

// Well-known header keys carried on every Envelope produced by this
// package.
//
//	HeaderKanbanKind  the EventKind constant (e.g. "kanban.task.submitted")
//	HeaderCardID      card_id (task / callback events only)
//	HeaderScheduleID  schedule_id (cron events only)
//
// Subscribers using a broad subject pattern (e.g. kanban.>) can route on
// these headers instead of re-parsing the subject.
const (
	HeaderKanbanKind = "kanban_kind"
	HeaderCardID     = "card_id"
	HeaderScheduleID = "schedule_id"
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

// stampVersion fills in the Version field of any known payload via a
// reflection-free type switch and returns the (possibly modified) value.
// Unknown types pass through unchanged.
func stampVersion(payload any) any {
	switch p := payload.(type) {
	case TaskSubmittedPayload:
		p.Version = payloadVersion
		return p
	case TaskClaimedPayload:
		p.Version = payloadVersion
		return p
	case TaskCompletedPayload:
		p.Version = payloadVersion
		return p
	case TaskFailedPayload:
		p.Version = payloadVersion
		return p
	case CallbackStartPayload:
		p.Version = payloadVersion
		return p
	case CallbackDonePayload:
		p.Version = payloadVersion
		return p
	case CronRuleCreatedPayload:
		p.Version = payloadVersion
		return p
	case CronRuleFiredPayload:
		p.Version = payloadVersion
		return p
	case CronRuleDisabledPayload:
		p.Version = payloadVersion
		return p
	}
	return payload
}

// publishCardEvent constructs and dispatches a card-scoped envelope.
// The subject is computed from kind + cardID; HeaderRunID-style well-known
// headers populate card_id and the kanban_kind label so subscribers using
// a coarse Pattern can still filter cheaply.
//
// Errors from Publish are intentionally swallowed to match the previous
// behaviour: Kanban state transitions must not fail because an observer's
// bus is overloaded.
func publishCardEvent(ctx context.Context, bus event.Bus, kind, cardID, scopeID string, payload any) {
	subject := cardSubjectFor(kind, cardID)
	if subject == "" {
		// Unknown kind — refuse to silently emit a malformed subject.
		return
	}
	env, err := event.NewEnvelope(ctx, subject, stampVersion(payload))
	if err != nil {
		return
	}
	env.SetHeader(HeaderKanbanKind, kind)
	if cardID != "" {
		env.SetHeader(HeaderCardID, cardID)
	}
	if scopeID != "" {
		env.Source = scopeID
	}
	_ = bus.Publish(ctx, env)
}

// publishCronEvent is the cron analogue of publishCardEvent.
func publishCronEvent(ctx context.Context, bus event.Bus, kind, scheduleID, scopeID string, payload any) {
	subject := cronSubjectFor(kind, scheduleID)
	if subject == "" {
		return
	}
	env, err := event.NewEnvelope(ctx, subject, stampVersion(payload))
	if err != nil {
		return
	}
	env.SetHeader(HeaderKanbanKind, kind)
	if scheduleID != "" {
		env.SetHeader(HeaderScheduleID, scheduleID)
	}
	if scopeID != "" {
		env.Source = scopeID
	}
	_ = bus.Publish(ctx, env)
}

// cardSubjectFor maps an EventKind constant to the corresponding card
// Subject helper. Returns "" for unknown kinds so the caller can refuse
// to publish a malformed subject.
func cardSubjectFor(kind, cardID string) event.Subject {
	switch kind {
	case EventTaskSubmitted:
		return subjTaskSubmitted(cardID)
	case EventTaskClaimed:
		return subjTaskClaimed(cardID)
	case EventTaskCompleted:
		return subjTaskCompleted(cardID)
	case EventTaskFailed:
		return subjTaskFailed(cardID)
	case EventCallbackStart:
		return subjCallbackStart(cardID)
	case EventCallbackDone:
		return subjCallbackDone(cardID)
	}
	return ""
}

// cronSubjectFor maps an EventKind cron constant to the corresponding
// cron Subject helper.
func cronSubjectFor(kind, scheduleID string) event.Subject {
	switch kind {
	case EventCronRuleCreated:
		return subjCronCreated(scheduleID)
	case EventCronRuleFired:
		return subjCronFired(scheduleID)
	case EventCronRuleDisabled:
		return subjCronDisabled(scheduleID)
	}
	return ""
}
