package kanban

// EventBus event types for Kanban operations.
const (
	EventTaskSubmitted = "kanban.task.submitted"
	EventTaskClaimed   = "kanban.task.claimed"
	EventTaskCompleted = "kanban.task.completed"
	EventTaskFailed    = "kanban.task.failed"
	EventCallbackStart = "kanban.callback.start"
	EventCallbackDone  = "kanban.callback.done"
)

// TaskSubmittedPayload is published when a task is submitted.
type TaskSubmittedPayload struct {
	CardID        string         `json:"card_id"`
	TargetAgentID string         `json:"target_agent_id"`
	Query         string         `json:"query"`
	RuntimeID     string         `json:"runtime_id"`
	Inputs        map[string]any `json:"inputs,omitempty"`
}

// TaskClaimedPayload is published when an agent claims a task.
type TaskClaimedPayload struct {
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
}

// TaskCompletedPayload is published when a task is completed.
type TaskCompletedPayload struct {
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
	Output        string `json:"output"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

// TaskFailedPayload is published when a task fails.
type TaskFailedPayload struct {
	CardID        string `json:"card_id"`
	TargetAgentID string `json:"target_agent_id"`
	RuntimeID     string `json:"runtime_id"`
	Error         string `json:"error"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

// CallbackStartPayload is published just before the callback message is sent
// to the Dispatcher Actor, so WS subscribers can start streaming.
type CallbackStartPayload struct {
	CardID    string `json:"card_id"`
	RuntimeID string `json:"runtime_id"`
	AgentID   string `json:"agent_id"`
	Query     string `json:"query"`
}

// CallbackDonePayload is published when the Dispatcher finishes processing
// a task callback, signaling the frontend to finalize the streamed message.
// Error is set when the callback turn ended unsuccessfully.
type CallbackDonePayload struct {
	CardID    string `json:"card_id"`
	RuntimeID string `json:"runtime_id"`
	AgentID   string `json:"agent_id"`
	Error     string `json:"error,omitempty"`
}
