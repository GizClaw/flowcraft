package azure

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
)

// SecretAPIKey is the Azure OpenAI resource key secret id.
const SecretAPIKey = "api_key"

// profileMaterial is the resolved credential set for one profile.
type profileMaterial struct {
	apiKey string
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	material := profileMaterial{}
	for id, secret := range profile.Secrets {
		switch id {
		case SecretAPIKey:
			material.apiKey = strings.TrimSpace(string(secret.Bytes()))
		default:
			return profileMaterial{}, fmt.Errorf(
				"azure: profile %q carries unknown secret %q (supported: api_key)",
				profile.ID,
				id,
			)
		}
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"azure: profile %q is missing the required api_key secret",
			profile.ID,
		)
	}
	return material, nil
}

// clients bundles the service handles one profile needs. Every operation
// surface shares the single typed SDK client.
type clients struct {
	api openaigo.Client
}

func (m profileMaterial) newClients(spec Spec) *clients {
	version := spec.APIVersion
	if version == "" {
		version = DefaultAPIVersion
	}
	options := []option.RequestOption{
		azure.WithEndpoint(strings.TrimSuffix(spec.Endpoint, "/"), version),
		azure.WithAPIKey(m.apiKey),
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
