package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
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

	options := []option.RequestOption{}
	switch spec.routing() {
	case routingAzureDeployment:
		// The SDK's Azure mode owns the parts that are easy to get wrong:
		// the Api-Key header, the api-version query, and the
		// /openai/deployments/{model}/... path rewriting for both JSON and
		// multipart routes.
		options = append(options,
			azure.WithEndpoint(
				strings.TrimSuffix(spec.baseURL(), "/"),
				spec.azureAPIVersion(),
			),
			azure.WithAPIKey(apiKey),
		)
	default:
		switch spec.authScheme() {
		case authNone:
			// No credential: drop the bearer header the SDK installs by
			// default so nothing rides the wire.
			options = append(options, option.WithHeaderDel("Authorization"))
		case authHeader:
			// A named header carries the key: drop the bearer header the
			// SDK installs by default so credentials never ride twice.
			options = append(options,
				option.WithHeaderDel("Authorization"),
				option.WithHeader(spec.Auth.Header, apiKey),
			)
		default:
			options = append(options, option.WithAPIKey(apiKey))
		}
		options = append(options, option.WithBaseURL(spec.baseURL()))
	}
	if spec.Endpoint.Organization != "" {
		options = append(options, option.WithOrganization(spec.Endpoint.Organization))
	}
	if spec.Endpoint.Project != "" {
		options = append(options, option.WithProject(spec.Endpoint.Project))
	}
	for name, value := range spec.Endpoint.Headers {
		options = append(options, option.WithHeader(name, value))
	}
	for name, value := range spec.Endpoint.Query {
		if spec.routing() == routingAzureDeployment && name == "api-version" {
			// azure.WithEndpoint already installs the version.
			continue
		}
		options = append(options, option.WithQueryAdd(name, value))
	}
	if spec.HTTPRetries != nil {
		options = append(options,
			option.WithMaxRetries(sdkMaxRetries(int(*spec.HTTPRetries))))
	}
	if timeout := spec.endpointTimeout(); timeout > 0 {
		options = append(options, option.WithRequestTimeout(timeout))
	}
	return &clients{api: openai.NewClient(options...)}, nil
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
