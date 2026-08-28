package anthropic

import (
	"context"
	"fmt"
	"strings"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the Anthropic API key secret id.
const SecretAPIKey = "api_key"

// profileMaterial is the resolved credential set for one profile.
type profileMaterial struct {
	apiKey   resource.Secret
	resolver *resource.SecretResolver
}

func newProfileMaterial(ctx context.Context, profile ProfileSettings, secrets *resource.SecretResolver) (profileMaterial, error) {
	material := profileMaterial{resolver: secrets}
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
		material.apiKey = secret
	}
	return material, nil
}

// clients bundles the service handles one profile needs.
type clients struct {
	api anthropicgo.Client
}

func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	apiKey, err := m.apiKey.Resolve(ctx, m.resolver)
	if err != nil {
		return nil, errdefs.Validationf("anthropic profile: resolve api_key: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errdefs.Validationf("anthropic profile is missing the required api_key secret")
	}
	options := []option.RequestOption{option.WithAPIKey(apiKey)}
	if spec.BaseURL != "" {
		options = append(options, option.WithBaseURL(spec.BaseURL))
	}
	if spec.HTTPRetries != nil {
		options = append(options,
			option.WithMaxRetries(sdkMaxRetries(int(*spec.HTTPRetries))))
	}
	return &clients{api: anthropicgo.NewClient(options...)}, nil
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
