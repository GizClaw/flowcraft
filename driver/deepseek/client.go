package deepseek

import (
	"fmt"
	"strings"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const defaultBaseURL = "https://api.deepseek.com"

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

// clients carries the SDK handle one profile opens drivers with. DeepSeek
// speaks the OpenAI-compatible chat and responses protocols, so the
// openai-go client does the HTTP work pointed at the DeepSeek base URL.
type clients struct {
	api openaigo.Client
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
				"deepseek profile %q carries unknown secret %q",
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
			"deepseek profile %q needs %q",
			profile.ID,
			SecretAPIKey,
		)
	}
	return material, nil
}

func (m profileMaterial) newClients(spec Spec) *clients {
	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	options := []option.RequestOption{
		option.WithAPIKey(m.apiKey),
		option.WithBaseURL(baseURL),
	}
	if spec.HTTPRetries != nil {
		options = append(options, option.WithMaxRetries(sdkMaxRetries(*spec.HTTPRetries)))
	}
	return &clients{api: openaigo.NewClient(options...)}
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
