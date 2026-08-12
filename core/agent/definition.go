package agent

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Definition is the document form of an agent: the identity card, the
// tool allow-list, the engine selection (itself a resource), the
// resource bindings, and the lifecycle hooks. The runtime form
// (identity, Engine, Host, Run) is separate.
type Definition struct {
	Card   Card              `json:"card"`
	Tools  []string          `json:"tools,omitempty"`
	Engine Engine            `json:"engine,omitzero"`
	Deps   resource.Deps     `json:"deps,omitempty"`
	Hooks  map[string][]Hook `json:"hooks,omitempty"`
}

// Validate checks the definition DTO.
func (d Definition) Validate() error {
	if strings.TrimSpace(d.Card.Name) == "" {
		return errdefs.Validationf("agent: card.name is required")
	}
	if err := d.Deps.Validate(); err != nil {
		return err
	}
	engineSet := d.Engine.Kind != "" || d.Engine.Impl != "" ||
		len(d.Engine.Deps) > 0 || len(d.Engine.Settings) > 0
	if engineSet {
		if err := d.Engine.Validate(); err != nil {
			return err
		}
	}
	for slot, entries := range d.Hooks {
		if strings.TrimSpace(slot) == "" {
			return errdefs.Validationf("agent: hook slot name is empty")
		}
		for i, hook := range entries {
			if err := hook.Validate(); err != nil {
				return errdefs.Validationf(
					"agent: hooks[%q][%d]: %v", slot, i, err)
			}
		}
	}
	return nil
}

// Card is the agent identity presented to hosts and telemetry.
type Card struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Engine selects the agent engine resource and its bindings.
type Engine struct {
	Kind     resource.Kind   `json:"kind"`
	Impl     string          `json:"impl,omitempty"`
	Deps     resource.Deps   `json:"deps,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Validate checks the engine DTO.
func (e Engine) Validate() error {
	if e.Kind == "" {
		return errdefs.Validationf("agent engine: kind is required")
	}
	return e.Deps.Validate()
}

// Hook is one agent lifecycle hook entry: the hook type (looked up in
// the resource registry under "hook.<slot>"), its resource bindings,
// and its opaque settings.
type Hook struct {
	Type     string          `json:"type"`
	Deps     resource.Deps   `json:"deps,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Validate checks the hook DTO.
func (h Hook) Validate() error {
	if strings.TrimSpace(h.Type) == "" {
		return errdefs.Validationf("hook: type is required")
	}
	if err := h.Deps.Validate(); err != nil {
		return err
	}
	if len(h.Settings) > 0 && !json.Valid(h.Settings) {
		return errdefs.Validationf("hook %q: settings is not valid JSON", h.Type)
	}
	return nil
}
