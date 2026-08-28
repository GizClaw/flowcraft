package minimax

import (
	"context"
	"fmt"
	"strings"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// defaultBaseURL is the China endpoint, matching the legacy minimax
// adapter; international deployments override base_url with
// https://api.minimax.io/anthropic.
const defaultBaseURL = "https://api.minimaxi.com/anthropic"

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec     ProfileSpec
	apiKey   resource.Secret
	resolver *resource.SecretResolver
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

func newProfileMaterial(ctx context.Context, profile ProfileSettings, secrets *resource.SecretResolver) (profileMaterial, error) {
	spec, err := decodeProfileSpec(ctx, profile.Spec)
	if err != nil {
		return profileMaterial{}, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	material := profileMaterial{spec: spec, resolver: secrets}
	for name := range profile.Secrets {
		switch name {
		case SecretAPIKey:
		default:
			return profileMaterial{}, fmt.Errorf("profile %q carries unknown secret %q", profile.ID, name)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = secret
	}
	return material, nil
}

func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	apiKey, err := m.apiKey.Resolve(ctx, m.resolver)
	if err != nil {
		return nil, errdefs.Validationf("minimax profile: resolve api_key: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errdefs.Validationf("minimax profile resolves no api_key secret")
	}
	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	options := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
	}
	if spec.HTTPRetries != nil {
		options = append(options,
			option.WithMaxRetries(sdkMaxRetries(int(*spec.HTTPRetries))))
	}
	return &clients{
		api:   anthropicgo.NewClient(options...),
		media: newMediaClient(apiKey, spec.mediaBaseURL(), spec),
	}, nil
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
