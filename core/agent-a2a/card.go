package a2a

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ProtocolVersion is the A2A version this adapter speaks.
const ProtocolVersion = "0.3.0"

// Skill is one advertised capability.
//
// It is not a core/skill document. A SKILL.md is a procedure the model loads on
// demand; a Skill here is a line on the card a registry routes by. They share a
// word and nothing else, which is why the two types never meet.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// CardConfig is what a deployment declares about its agent.
type CardConfig struct {
	// Name and Description identify the agent to a client.
	Name        string
	Description string
	// Version is the agent's own version, not the protocol's.
	Version string
	// URL is where the agent is reachable, when the host serves it.
	URL string
	// Skills are advertised capabilities. They are claims, so they are declared
	// rather than derived.
	Skills []Skill
	// DefaultInputModes and DefaultOutputModes are media types
	// ("text/plain", "application/json"). Empty means the deployment made no
	// claim, which is honest; filling them from model content kinds would be a
	// guess about a different layer.
	DefaultInputModes  []string
	DefaultOutputModes []string
	// PushNotifications and StateTransitionHistory are service capabilities the
	// host either provides or does not.
	PushNotifications      bool
	StateTransitionHistory bool
}

// Card is the A2A AgentCard.
type Card struct {
	ProtocolVersion    string       `json:"protocolVersion"`
	Name               string       `json:"name"`
	Description        string       `json:"description,omitempty"`
	URL                string       `json:"url,omitempty"`
	Version            string       `json:"version,omitempty"`
	Skills             []Skill      `json:"skills,omitempty"`
	DefaultInputModes  []string     `json:"defaultInputModes,omitempty"`
	DefaultOutputModes []string     `json:"defaultOutputModes,omitempty"`
	Capabilities       Capabilities `json:"capabilities"`
}

// Capabilities declares optional protocol features. Streaming is not a choice:
// the loop only produces an event stream.
type Capabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications,omitempty"`
	StateTransitionHistory bool `json:"stateTransitionHistory,omitempty"`
}

// NewCard builds the card a deployment serves.
func NewCard(cfg CardConfig) (Card, error) {
	if cfg.Name == "" {
		return Card{}, errdefs.Validationf("a2a: card name is required")
	}
	seen := make(map[string]struct{}, len(cfg.Skills))
	for i, skill := range cfg.Skills {
		if skill.ID == "" || skill.Name == "" {
			return Card{}, errdefs.Validationf(
				"a2a: skills[%d] needs an id and a name", i)
		}
		if _, duplicate := seen[skill.ID]; duplicate {
			return Card{}, errdefs.Validationf(
				"a2a: skills[%d]: duplicate id %q", i, skill.ID)
		}
		seen[skill.ID] = struct{}{}
	}
	return Card{
		ProtocolVersion:    ProtocolVersion,
		Name:               cfg.Name,
		Description:        cfg.Description,
		URL:                cfg.URL,
		Version:            cfg.Version,
		Skills:             cfg.Skills,
		DefaultInputModes:  cfg.DefaultInputModes,
		DefaultOutputModes: cfg.DefaultOutputModes,
		Capabilities: Capabilities{
			// A fact about this implementation, not a deployment choice.
			Streaming:              true,
			PushNotifications:      cfg.PushNotifications,
			StateTransitionHistory: cfg.StateTransitionHistory,
		},
	}, nil
}
