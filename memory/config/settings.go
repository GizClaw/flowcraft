package config

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// ModelSettings is the flat JSON form of an exact inference model reference:
// the same fields as inference.ModelRef, serialized directly instead of
// nested under an id object.
type ModelSettings struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Profile  string `json:"profile,omitempty"`
}

// Ref converts the flat settings form back to the SDK model reference.
func (m ModelSettings) Ref() inference.ModelRef {
	return inference.ModelRef{ID: inference.ModelID{Provider: m.Provider, Name: m.Name}, Profile: m.Profile}
}

// FromModelRef converts an SDK model reference to its flat settings form.
func FromModelRef(ref inference.ModelRef) ModelSettings {
	return ModelSettings{Provider: ref.ID.Provider, Name: ref.ID.Name, Profile: ref.Profile}
}

// ScopeSettings is one optional hard-scope seed registered before worker scans.
type ScopeSettings struct {
	RuntimeID string `json:"runtime_id"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

func (s ScopeSettings) scope() sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: s.RuntimeID, UserID: s.UserID, AgentID: s.AgentID}
}
