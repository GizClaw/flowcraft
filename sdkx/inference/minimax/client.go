package minimax

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultBaseURL is the China endpoint, matching the legacy minimax
// adapter; international deployments override base_url with
// https://api.minimax.io/anthropic.
const defaultBaseURL = "https://api.minimaxi.com/anthropic"

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

// clients carries the handles one profile opens drivers with. MiniMax
// serves the Anthropic Messages protocol — signed thinking blocks and
// all — so the anthropic-go client does the Messages HTTP work; the media
// APIs (t2a, video, image) ride a plain JSON client rooted at the media
// base URL.
type clients struct {
	api   anthropicgo.Client
	media *mediaClient
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
		options = append(options, option.WithMaxRetries(sdkMaxRetries(*spec.HTTPRetries)))
	}
	return &clients{
		api:   anthropicgo.NewClient(options...),
		media: newMediaClient(m.apiKey, spec.mediaBaseURL(), spec),
	}
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
