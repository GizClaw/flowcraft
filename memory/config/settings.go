package config

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// ModelSettings is the JSON-friendly exact inference model reference.
type ModelSettings struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Profile  string `json:"profile,omitempty"`
}

func (m ModelSettings) ref() inference.ModelRef {
	return inference.ModelRef{ID: inference.ModelID{Provider: m.Provider, Name: m.Name}, Profile: m.Profile}
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
