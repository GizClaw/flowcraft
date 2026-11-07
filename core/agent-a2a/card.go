package a2a

import (
	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Skill is one advertised capability.
//
// It is not a core/skill document. A SKILL.md is a procedure the model loads on
// demand; a Skill here is a line on the card a registry routes by. They share a
// word and nothing else.
type Skill struct {
	ID          string
	Name        string
	Description string
	Tags        []string
	Examples    []string
}

// CardConfig is what a deployment declares about its agent.
type CardConfig struct {
	// Name, Description and Version identify the agent to a client.
	Name        string
	Description string
	Version     string
	// URL is where the agent is reachable over JSON-RPC.
	URL string
	// Skills are advertised capabilities: claims, so declared.
	Skills []Skill
	// DefaultInputModes and DefaultOutputModes are media types
	// ("text/plain", "application/json"). Empty means the deployment made no
	// claim, which is honest; model content kinds answer a different question.
	DefaultInputModes  []string
	DefaultOutputModes []string
	// PushNotifications is a service capability the host either provides or not.
	PushNotifications bool
}

// NewCard builds the AgentCard the server answers with.
func NewCard(cfg CardConfig) (*a2a.AgentCard, error) {
	if cfg.Name == "" {
		return nil, errdefs.Validationf("a2a: card name is required")
	}
	if cfg.Version == "" {
		return nil, errdefs.Validationf("a2a: card version is required")
	}
	if cfg.URL == "" {
		return nil, errdefs.Validationf("a2a: card url is required")
	}
	skills := make([]a2a.AgentSkill, 0, len(cfg.Skills))
	seen := make(map[string]struct{}, len(cfg.Skills))
	for i, skill := range cfg.Skills {
		if skill.ID == "" || skill.Name == "" {
			return nil, errdefs.Validationf("a2a: skills[%d] needs an id and a name", i)
		}
		if _, duplicate := seen[skill.ID]; duplicate {
			return nil, errdefs.Validationf("a2a: skills[%d]: duplicate id %q", i, skill.ID)
		}
		seen[skill.ID] = struct{}{}
		skills = append(skills, a2a.AgentSkill{
			ID:          skill.ID,
			Name:        skill.Name,
			Description: skill.Description,
			Tags:        skill.Tags,
			Examples:    skill.Examples,
		})
	}
	return &a2a.AgentCard{
		Name:        cfg.Name,
		Description: cfg.Description,
		Version:     cfg.Version,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(cfg.URL, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  cfg.DefaultInputModes,
		DefaultOutputModes: cfg.DefaultOutputModes,
		Skills:             skills,
		Capabilities: a2a.AgentCapabilities{
			// A fact about the loop, not a deployment choice.
			Streaming:         true,
			PushNotifications: cfg.PushNotifications,
		},
	}, nil
}
