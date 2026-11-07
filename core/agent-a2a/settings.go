package a2a

import (
	"bytes"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"go.yaml.in/yaml/v3"
)

// ServerSettings is the typed form of a deployment's protocol block:
//
//	protocol:
//	  kind: a2a
//	  settings:
//	    card: {description: ..., version: "1.0.0", url: https://...}
type ServerSettings struct {
	Card CardConfig
}

// settingsDocument mirrors the written block, so unknown keys are rejected.
type settingsDocument struct {
	Card cardDocument `yaml:"card"`
}

type cardDocument struct {
	Name               string          `yaml:"name"`
	Description        string          `yaml:"description"`
	Version            string          `yaml:"version"`
	URL                string          `yaml:"url"`
	Skills             []skillDocument `yaml:"skills"`
	DefaultInputModes  []string        `yaml:"defaultInputModes"`
	DefaultOutputModes []string        `yaml:"defaultOutputModes"`
	PushNotifications  bool            `yaml:"pushNotifications"`
}

type skillDocument struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Examples    []string `yaml:"examples"`
}

// DecodeSettings validates a protocol block at load time.
//
// The card's name may be omitted: an agent's card is named after the agent, and
// only the builder knows that name. Everything else — the version it advertises,
// the URL clients will call, the skills it claims — must be declared here,
// because a card is a promise and a promise with a default is not one.
func DecodeSettings(settings map[string]any) (ServerSettings, error) {
	if len(settings) == 0 {
		return ServerSettings{}, errdefs.Validationf(
			"a2a: settings.card is required; a server has to advertise something")
	}
	body, err := yaml.Marshal(settings)
	if err != nil {
		return ServerSettings{}, errdefs.Validationf("a2a: encode settings: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var document settingsDocument
	if err := decoder.Decode(&document); err != nil {
		return ServerSettings{}, errdefs.Validationf("a2a: settings: %v", err)
	}
	card := document.Card
	if card.Version == "" {
		return ServerSettings{}, errdefs.Validationf("a2a: card.version is required")
	}
	if card.URL == "" {
		return ServerSettings{}, errdefs.Validationf("a2a: card.url is required")
	}
	skills := make([]Skill, 0, len(card.Skills))
	seen := make(map[string]struct{}, len(card.Skills))
	for i, skill := range card.Skills {
		if skill.ID == "" || skill.Name == "" {
			return ServerSettings{}, errdefs.Validationf(
				"a2a: card.skills[%d] needs an id and a name", i)
		}
		if _, duplicate := seen[skill.ID]; duplicate {
			return ServerSettings{}, errdefs.Validationf(
				"a2a: card.skills[%d]: duplicate id %q", i, skill.ID)
		}
		seen[skill.ID] = struct{}{}
		// Struct tags are ignored for conversion, so the mirrored document type
		// converts directly.
		skills = append(skills, Skill(skill))
	}
	return ServerSettings{Card: CardConfig{
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
