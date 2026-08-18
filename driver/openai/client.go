package openai

import (
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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

func newProfileMaterial(profile ProfileSettings) (profileMaterial, error) {
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
		material.apiKey = strings.TrimSpace(secret)
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
	if spec.HTTPRetries != nil {
		options = append(options,
			option.WithMaxRetries(sdkMaxRetries(int(*spec.HTTPRetries))))
	}
	return &clients{api: openai.NewClient(options...)}
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
