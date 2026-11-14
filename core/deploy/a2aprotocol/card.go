package a2aprotocol

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/goccy/go-json"
)

// cardSkill is one advertised capability.
//
// It is not a core/skill document. A SKILL.md is a procedure the model loads on
// demand; a skill here is a line on the card a registry routes by. They share a
// word and nothing else.
type cardSkill struct {
	ID          string
	Name        string
	Description string
	Tags        []string
	Examples    []string
}

// cardConfig is what a deployment declares about its agent.
type cardConfig struct {
	// Name, Description and Version identify the agent to a client.
	Name        string
	Description string
	Version     string
	// URL is where the agent is reachable over JSON-RPC.
	URL string
	// Skills are advertised capabilities: claims, so declared.
	Skills []cardSkill
	// DefaultInputModes and DefaultOutputModes are media types
	// ("text/plain", "application/json"). Empty means the deployment made no
	// claim, which is honest; model content kinds answer a different question.
	DefaultInputModes  []string
	DefaultOutputModes []string
	// PushNotifications is a service capability the host either provides or not.
	PushNotifications bool
}

// cardSettings is the typed form of a deployment's protocol block:
//
//	protocol:
//	  kind: a2a
//	  settings:
//	    card: {description: ..., version: "1.0.0", url: https://...}
type cardSettings struct {
	Card cardConfig
}

// settingsDocument mirrors the written block, so unknown keys are rejected.
type settingsDocument struct {
	Card cardDocument `json:"card"`
}

type cardDocument struct {
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Version            string          `json:"version"`
	URL                string          `json:"url"`
	Skills             []skillDocument `json:"skills"`
	DefaultInputModes  []string        `json:"defaultInputModes"`
	DefaultOutputModes []string        `json:"defaultOutputModes"`
	PushNotifications  bool            `json:"pushNotifications"`
}

type skillDocument struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples"`
}

// DecodeSettings validates a protocol block at load time.
//
// The card's name may be omitted: an agent's card is named after the agent, and
// only the builder knows that name. Everything else — the version it advertises,
// the URL clients will call, the skills it claims — must be declared here,
// because a card is a promise and a promise with a default is not one.
func DecodeSettings(raw json.RawMessage) (cardSettings, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return cardSettings{}, errdefs.Validationf(
			"a2a: settings.card is required; a server has to advertise something")
	}
	document, err := decode.Decode[settingsDocument](raw)
	if err != nil {
		return cardSettings{}, errdefs.Validationf("a2a: settings: %v", err)
	}
	card := document.Card
	if card.Version == "" {
		return cardSettings{}, errdefs.Validationf("a2a: card.version is required")
	}
	if card.URL == "" {
		return cardSettings{}, errdefs.Validationf("a2a: card.url is required")
	}
	skills := make([]cardSkill, 0, len(card.Skills))
	seen := make(map[string]struct{}, len(card.Skills))
	for i, skill := range card.Skills {
		if skill.ID == "" || skill.Name == "" {
			return cardSettings{}, errdefs.Validationf(
				"a2a: card.skills[%d] needs an id and a name", i)
		}
		if _, duplicate := seen[skill.ID]; duplicate {
			return cardSettings{}, errdefs.Validationf(
				"a2a: card.skills[%d]: duplicate id %q", i, skill.ID)
		}
		seen[skill.ID] = struct{}{}
		// Struct tags are ignored for conversion, so the mirrored document type
		// converts directly.
		skills = append(skills, cardSkill(skill))
	}
	return cardSettings{Card: cardConfig{
		Name:               card.Name,
		Description:        card.Description,
		Version:            card.Version,
		URL:                card.URL,
		Skills:             skills,
		DefaultInputModes:  card.DefaultInputModes,
		DefaultOutputModes: card.DefaultOutputModes,
		PushNotifications:  card.PushNotifications,
	}}, nil
}

// newCard builds the AgentCard the server answers with.
func newCard(cfg cardConfig) (*a2a.AgentCard, error) {
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
