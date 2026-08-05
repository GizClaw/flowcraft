package config

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// ModelSettings is the YAML-friendly exact inference model reference.
type ModelSettings struct {
	Provider string `yaml:"provider"`
	Name     string `yaml:"name"`
	Profile  string `yaml:"profile,omitempty"`
}

func (m ModelSettings) ref() inference.ModelRef {
	return inference.ModelRef{ID: inference.ModelID{Provider: m.Provider, Name: m.Name}, Profile: m.Profile}
}

// ScopeSettings is one optional hard-scope seed registered before worker scans.
type ScopeSettings struct {
	RuntimeID string `yaml:"runtime_id"`
	UserID    string `yaml:"user_id,omitempty"`
	AgentID   string `yaml:"agent_id,omitempty"`
}

func (s ScopeSettings) scope() sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: s.RuntimeID, UserID: s.UserID, AgentID: s.AgentID}
}
