package deepseek

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"

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

// clients carries the SDK handles one profile opens drivers with. DeepSeek
// speaks the OpenAI chat-completions protocol, so the openai-go client does
// the HTTP work pointed at the DeepSeek base URL; DeepSeek-only request
// fields (thinking, reasoning_effort) ride per-request JSON overrides and
// response extras (reasoning_content) read back through the SDK's raw JSON
// metadata.
type clients struct {
	api openaigo.Client
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	material := profileMaterial{spec: spec}
	for name, secret := range profile.Secrets {
		switch name {
		case SecretAPIKey:
			material.apiKey = secretString(secret)
		default:
			return profileMaterial{}, fmt.Errorf("profile %q carries unknown secret %q", profile.ID, name)
		}
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf("profile %q resolves no api_key secret", profile.ID)
	}
	return material, nil
}

func secretString(secret config.Secret) string {
	return strings.TrimSpace(string(secret.Bytes()))
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
		options = append(options, sdkMaxRetriesOption(*spec.HTTPRetries))
	}
	return &clients{
		api: openaigo.NewClient(options...),
	}
}

// sdkMaxRetriesOption converts a total-attempt budget (including the first)
// into the openai-go retry-count option.
func sdkMaxRetriesOption(total int) option.RequestOption {
	if total <= 1 {
		return option.WithMaxRetries(0)
	}
	return option.WithMaxRetries(total - 1)
}
