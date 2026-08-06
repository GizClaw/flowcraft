package openai

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// profileMaterial is one credential profile after secret resolution: the
// decoded profile Spec plus the secret values this provider recognizes.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

// clients bundles the service handles one profile needs. Every operation
// surface shares the single typed SDK client today.
type clients struct {
	api openai.Client
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec}
	for name := range profile.Secrets {
		if name != SecretAPIKey {
			return profileMaterial{}, fmt.Errorf(
				"openai profile %q carries unknown secret %q",
				profile.ID,
				name,
			)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = secretString(secret)
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"openai profile %q needs %q",
			profile.ID,
			SecretAPIKey,
		)
	}
	return material, nil
}

// secretString converts a secret payload to a single-line credential value.
func secretString(secret config.Secret) string {
	return strings.TrimSpace(string(secret.Bytes()))
}

// newClients builds the service handles for one profile.
func (m profileMaterial) newClients(spec Spec) *clients {
	options := []option.RequestOption{option.WithAPIKey(m.apiKey)}
	if spec.BaseURL != "" {
		options = append(options, option.WithBaseURL(spec.BaseURL))
	}
	if spec.Organization != "" {
		options = append(options, option.WithOrganization(spec.Organization))
	}
	if spec.Project != "" {
		options = append(options, option.WithProject(spec.Project))
	}
	return &clients{api: openai.NewClient(options...)}
}
