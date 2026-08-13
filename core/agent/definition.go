package agent

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Hook slot names. A hook factory registers under ("hook."+slot,
// type); the document lists hooks under the matching top-level field
// (prepare / observe / referees / commit). The four slots mirror the
// lifecycle stages in core/agent: Preparer, Observer, Referee,
// Committer.
const (
	HookSlotPreparer  = "prepare"
	HookSlotObserver  = "observe"
	HookSlotReferee   = "referee"
	HookSlotCommitter = "commit"
)

// Definition is the document form of an agent: the identity card, the
// tool allow-list, the engine selection (itself a resource), the
// resource bindings, and the lifecycle hooks by slot. The assembled
// runtime form is [Agent], built from a Definition by the deployment
// layer.
type Definition struct {
	Card     AgentCard `json:"card,omitzero"`
	Tools    []string  `json:"tools,omitempty"`
	Engine   EngineRef `json:"engine,omitzero"`
	Policy   *Policy   `json:"policy,omitempty"`
	Prepare  []Hook    `json:"prepare,omitempty"`
	Observe  []Hook    `json:"observe,omitempty"`
	Referees []Hook    `json:"referees,omitempty"`
	Commit   []Hook    `json:"commit,omitempty"`
}

// Validate checks the definition DTO.
func (d Definition) Validate() error {
	if err := d.Card.Validate(); err != nil {
		return err
	}
	engineSet := d.Engine.Kind != "" || d.Engine.Impl != "" ||
		len(d.Engine.Deps) > 0 || len(d.Engine.Settings) > 0
	if engineSet {
		if err := d.Engine.Validate(); err != nil {
			return err
		}
	}
	if d.Policy != nil && d.Policy.MaxRevise < 0 {
		return errdefs.Validationf(
			"agent: policy.max_revise must not be negative")
	}
	for _, list := range []struct {
		slot  string
		hooks []Hook
	}{
		{HookSlotPreparer, d.Prepare},
		{HookSlotObserver, d.Observe},
		{HookSlotReferee, d.Referees},
		{HookSlotCommitter, d.Commit},
	} {
		for i, hook := range list.hooks {
			if err := hook.Validate(); err != nil {
				return errdefs.Validationf(
					"agent: hooks[%q][%d]: %v", list.slot, i, err)
			}
		}
	}
	return nil
}

// Policy is the per-call harness policy declared on the document form
// and applied by Execute unless overridden per call.
type Policy struct {
	// MaxRevise bounds how many times a Referee may ask Execute to
	// re-invoke the engine. Zero means one attempt (no revise).
	MaxRevise int `json:"max_revise,omitempty"`
	// ArtifactChannels names board channels collected into
	// Result.Artifacts.
	ArtifactChannels []string `json:"artifact_channels,omitempty"`
}

// AgentCard describes an agent's capabilities for discovery. Field
// names and JSON tags are a proper subset of the A2A AgentCard
// specification: every field marshals to the exact key A2A readers
// expect, and AgentCard never collides with an A2A field by using a
// different name.
//
// What is intentionally NOT here: url / version / provider /
// documentationUrl / authentication — these belong to "how the agent
// is exposed as a service", a deployment concern owned by the
// protocol layer.
type AgentCard struct {
	Name               string            `json:"name"`
	Description        string            `json:"description,omitempty"`
	Skills             []Skill           `json:"skills,omitempty"`
	DefaultInputModes  []string          `json:"defaultInputModes,omitempty"`
	DefaultOutputModes []string          `json:"defaultOutputModes,omitempty"`
	Capabilities       AgentCapabilities `json:"capabilities,omitempty"`
}

// Validate checks the card invariants: a name and well-formed skills.
func (c AgentCard) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errdefs.Validationf("agent: card.name is required")
	}
	for i, skill := range c.Skills {
		if strings.TrimSpace(skill.ID) == "" {
			return errdefs.Validationf(
				"agent: card.skills[%d].id is required", i)
		}
	}
	return nil
}

// Skill is a single capability unit declared on an [AgentCard]. Field
// names mirror A2A's skill object so cards round-trip cleanly through
// /.well-known/agent-card.json.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// AgentCapabilities declares which optional A2A protocol features the
// agent supports. JSON keys exactly match the A2A spec — note the
// plural PushNotifications and the longer StateTransitionHistory.
type AgentCapabilities struct {
	Streaming              bool `json:"streaming,omitempty"`
	PushNotifications      bool `json:"pushNotifications,omitempty"`
	StateTransitionHistory bool `json:"stateTransitionHistory,omitempty"`
}

// EngineRef selects the agent engine resource and its bindings. It is
// the document-layer reference: the runtime execution contract will be
// a separate [Engine] interface (not yet in core), so the reference
// deliberately does not reuse that name.
type EngineRef struct {
	Kind     resource.Kind   `json:"kind"`
	Impl     string          `json:"impl,omitempty"`
	Deps     resource.Deps   `json:"deps,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Validate checks the engine reference DTO.
func (e EngineRef) Validate() error {
	if e.Kind == "" {
		return errdefs.Validationf("agent engine: kind is required")
	}
	return e.Deps.Validate()
}

// Hook is one agent lifecycle hook entry: the hook type (looked up in
// the resource registry under "hook.<slot>"), its resource bindings,
// and its opaque settings. All four slots share the same data shape.
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
