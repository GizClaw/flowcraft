package azure

import (
	"context"
	"fmt"
	"strings"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the Azure OpenAI resource key secret id.
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
				"azure: profile %q carries unknown secret %q (supported: api_key)",
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
	api openaigo.Client
}

func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	apiKey, err := m.apiKey.Resolve(ctx, m.resolver)
	if err != nil {
		return nil, errdefs.Validationf("azure profile: resolve api_key: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errdefs.Validationf("azure profile is missing the required api_key secret")
	}
	version := spec.APIVersion
	if version == "" {
		version = DefaultAPIVersion
	}
	options := []option.RequestOption{
		azure.WithEndpoint(strings.TrimSuffix(spec.Endpoint, "/"), version),
		azure.WithAPIKey(apiKey),
	}
	if spec.HTTPRetries != nil {
		options = append(options,
			sdkMaxRetriesOption(int(*spec.HTTPRetries)))
	}
	return &clients{api: openaigo.NewClient(options...)}, nil
}

// sdkMaxRetriesOption converts a total-attempt budget (including the first)
// into the openai-go retry-count option.
func sdkMaxRetriesOption(total int) option.RequestOption {
	if total <= 1 {
		return option.WithMaxRetries(0)
	}
	return option.WithMaxRetries(total - 1)
}
