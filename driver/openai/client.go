package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Secret names owned by this provider. Profile secrets outside this set are
// rejected at build time so typos fail fast instead of silently missing.
const (
	// SecretAPIKey authenticates every OpenAI-wire API surface.
	SecretAPIKey = "api_key"
)

// profileMaterial is one credential profile after secret resolution: the
// decoded profile Spec plus the secret values this provider recognizes.
type profileMaterial struct {
	spec     ProfileSpec
	apiKey   resource.Secret
	resolver *resource.SecretResolver
}

// clients bundles the service handles one profile needs. Every operation
// surface shares the single typed SDK client today.
type clients struct {
	api openai.Client
}

func newProfileMaterial(ctx context.Context, profile ProfileSettings, secrets *resource.SecretResolver) (profileMaterial, error) {
	spec, err := decodeProfileSpec(ctx, profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec, resolver: secrets}
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
		material.apiKey = secret
	}
	return material, nil
}

// newClients builds the service handles for one profile. The endpoint and
// auth blocks decide how a request travels; nothing here changes how a
// request is compiled.
func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	var apiKey string
	if spec.authScheme() != authNone {
		resolved, err := m.apiKey.Resolve(ctx, m.resolver)
		if err != nil {
			return nil, errdefs.Validationf(
				"openai profile: resolve api_key: %v", err)
		}
		apiKey = strings.TrimSpace(resolved)
		if apiKey == "" {
			return nil, errdefs.Validationf(
				"openai profile needs %q", SecretAPIKey)
		}
	}

	options := spec.requestOptions(apiKey)
	return &clients{api: openai.NewClient(options...)}, nil
}
