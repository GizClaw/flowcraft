package minimax

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// defaultMediaBaseURL is the China media root (t2a, video, image, music);
// international deployments override media_base_url with
// https://api.minimax.io. The Messages endpoint lives in the anthropic driver.
const defaultMediaBaseURL = "https://api.minimaxi.com"

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec     ProfileSpec
	apiKey   resource.Secret
	resolver *resource.SecretResolver
}

// clients carries the handles one profile opens drivers with. Every
// remaining surface is a MiniMax-native API, so one plain JSON client rooted
// at the media base URL is all it takes.
type clients struct {
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
	return &clients{
		media: newMediaClient(apiKey, spec.mediaBaseURL(), spec),
	}, nil
}
