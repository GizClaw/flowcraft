package anthropic

import (
	"fmt"
	"strings"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// SecretAPIKey is the Anthropic API key secret id.
const SecretAPIKey = "api_key"

// profileMaterial is the resolved credential set for one profile.
type profileMaterial struct {
	apiKey string
}

func newProfileMaterial(profile ProfileSettings) (profileMaterial, error) {
	material := profileMaterial{}
	for id := range profile.Secrets {
		if id != SecretAPIKey {
			return profileMaterial{}, fmt.Errorf(
				"anthropic: profile %q carries unknown secret %q (supported: api_key)",
				profile.ID,
				id,
			)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = strings.TrimSpace(secret)
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"anthropic: profile %q is missing the required api_key secret",
			profile.ID,
		)
	}
	return material, nil
}

// clients bundles the service handles one profile needs.
type clients struct {
	api anthropicgo.Client
}

func (m profileMaterial) newClients(spec Spec) *clients {
	options := []option.RequestOption{option.WithAPIKey(m.apiKey)}
	if spec.BaseURL != "" {
		options = append(options, option.WithBaseURL(spec.BaseURL))
	}
	if spec.HTTPRetries != nil {
		options = append(options, option.WithMaxRetries(sdkMaxRetries(*spec.HTTPRetries)))
	}
	return &clients{api: anthropicgo.NewClient(options...)}
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
