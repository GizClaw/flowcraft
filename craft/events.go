package craft

import "github.com/GizClaw/flowcraft/core/event"

// Reason classifies why a runtime was reloaded.
type Reason string

const (
	ReasonManual Reason = "manual"
	ReasonConfig Reason = "config"
	ReasonPlugin Reason = "plugin"
	ReasonAgent  Reason = "agent"
)

// Craft-plane subjects.
const (
	SubjectCraftStarted    = event.Subject("craft.started")
	SubjectRuntimeOpened   = event.Subject("craft.runtime.opened")
	SubjectRuntimeClosed   = event.Subject("craft.runtime.closed")
	SubjectReloadStarted   = event.Subject("craft.reload.started")
	SubjectReloadCompleted = event.Subject("craft.reload.completed")
	SubjectReloadFailed    = event.Subject("craft.reload.failed")
	SubjectManagerState    = event.Subject("craft.manager.state")
	SubjectGroupInstance   = event.Subject("craft.group.instance.state")
)

// PatternCraft matches every craft-plane event.
func PatternCraft() event.Pattern { return event.Pattern("craft.>") }

// RuntimeEvent is the payload of craft.runtime.* and craft.reload.*
// events.
type RuntimeEvent struct {
	Key    RuntimeKey `json:"key"`
	Reason Reason     `json:"reason,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// ManagerStateEvent is the payload of craft.manager.state.
type ManagerStateEvent struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// GroupInstanceEvent is the payload of craft.group.instance.state.
type GroupInstanceEvent struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
}
