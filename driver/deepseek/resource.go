package deepseek

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// DeepSeek provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one DeepSeek provider
// resource: the provider identity, credential-free spec, and one entry
// per credential profile. Secret values may carry ${env:NAME} references,
// resolved by the driver at build time.
type ResourceSettings struct {
	// ID is the stable provider identity used by model refs and the
	// inference assembly (e.g. "deepseek").
	ID string `json:"id"`
	// Spec is the provider-owned, credential-free configuration.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Profiles declares one credential profile per API key/account.
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile. Secrets maps the
// provider-owned secret name (api_key) to a resolved or ${env:NAME}
// referenced value.
type ProfileSettings struct {
	ID         string                `json:"id,omitempty"`
	Operations []inference.Operation `json:"operations,omitempty"`
	Secrets    map[string]string     `json:"secrets,omitempty"`
	Spec       json.RawMessage       `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the DeepSeek provider deployment factory.
func Factory() resource.Factory {
	return deployFactory{}
}

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "deepseek"}
}

// New implements resource.Factory: it strictly decodes the provider
// settings, resolves ${env:...} secret references, and builds the
// immutable core/inference.ProviderDefinition.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](
		in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, fmt.Errorf("deepseek provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("deepseek provider: settings.id is required")
	}
	return buildProvider(settings)
}

// Register adds the DeepSeek provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

// buildProvider builds the DeepSeek provider definition from one
// deployment provider config. It validates the provider Spec, merges the
// model catalog, resolves every credential profile, and binds each catalog
// model to the openers its kind serves. Unknown models fail closed: only
// catalog models (built-in or declared via Spec.Models) are exposed. The
// definition carries the generate_options decoder bound to the deployment
// provider ID, so graph yaml can enable provider knobs (web_search) without
// host-side registration.
func buildProvider(settings ResourceSettings) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(settings.Spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	if spec.apiMode() == apiResponses {
		for name, entry := range models {
			if !entry.responses {
				if entry.declared {
					return inference.ProviderDefinition{}, fmt.Errorf(
						"deepseek provider %q: model %q does not support the Responses API",
						settings.ID,
						name,
					)
				}
				// Built-in models without the responses capability are
				// excluded from a Responses provider rather than failing
				// the whole deployment.
				delete(models, name)
			}
		}
		if len(models) == 0 {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"deepseek provider %q: Responses API requires at least one responses-capable model",
				settings.ID,
			)
		}
	}
	profiles := make(map[string]profileMaterial, len(settings.Profiles))
	for _, profile := range settings.Profiles {
		material, err := newProfileMaterial(profile)
		if err != nil {
			return inference.ProviderDefinition{}, err
		}
		profiles[profile.ID] = material
	}

	provider := inference.ProviderDefinition{
		ID: settings.ID,
		ExtensionDecoders: map[string]inference.ExtensionDecoder{
			extensionGenerate: inference.ExtensionDecoderFor(func() *GenerateOptions {
				// Bind the deployment provider ID: the identity check in
				// DecodeExtensions compares against the wire provider, which
				// for graph yaml is the configured provider ID.
				return &GenerateOptions{Provider: settings.ID}
			}),
		},
	}
	for _, profile := range settings.Profiles {
		provider.Profiles = append(
			provider.Profiles,
			inference.ProfileDefinition{
				ID:         profile.ID,
				Operations: append([]inference.Operation(nil), profile.Operations...),
			},
		)
	}
	for _, name := range sortedNames(models) {
		entry := models[name]
		entry.api = spec.apiMode()
		id := inference.ModelID{Provider: settings.ID, Name: name}
		descriptor := inference.ModelDescriptor{
			ID: id,
			Capabilities: inference.ModelCapabilities{
				HostedWebSearch: entry.api == apiResponses && entry.webSearch,
			},
		}
		if entry.maxInputTokens > 0 {
			descriptor.Limits.MaxInputTokens = &entry.maxInputTokens
		}
		provider.Models = append(provider.Models, inference.ModelImplementation{
			Descriptor: descriptor,
			Openers:    openersFor(spec, entry, profiles, id),
		})
	}
	return provider, nil
}

// openersFor binds one catalog model to the generate opener for its kind.
// DeepSeek exposes only text generate today.
func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	open := func(profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"deepseek model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(spec), nil
	}
	if entry.kind != kindGenerate {
		return inference.Openers{}
	}
	return inference.Openers{
		Generate: func(
			_ context.Context,
			model inference.ModelRef,
		) (inference.GenerateOperations, error) {
			cls, err := open(model.Profile)
			if err != nil {
				return inference.GenerateOperations{}, err
			}
			return openGenerate(cls, entry, id, model.Profile)
		},
	}
}
