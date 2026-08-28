package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// OpenAI provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one OpenAI provider
// resource: the provider identity, credential-free spec, and one entry
// per credential profile. Secret values may carry ${env:NAME}
// references, resolved by the driver at build time.
type ResourceSettings struct {
	// ID is the stable provider identity used by model refs and the
	// inference assembly (e.g. "openai").
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
	ID         string                     `json:"id,omitempty"`
	Operations []inference.Operation      `json:"operations,omitempty"`
	Secrets    map[string]resource.Secret `json:"secrets,omitempty"`
	Spec       json.RawMessage            `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the OpenAI provider deployment factory.
func Factory() resource.Factory {
	return deployFactory{}
}

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "openai"}
}

// New implements resource.Factory: it strictly decodes the provider
// settings, resolves ${env:...} secret references, and builds the
// immutable core/inference.ProviderDefinition.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("openai provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("openai provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the OpenAI provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

// buildProvider builds the OpenAI provider definition from one
// deployment provider config. It validates the provider Spec, merges
// the model catalog, resolves every credential profile, and binds each
// catalog model to the openers its kind serves. Unknown models fail
// closed: only catalog models (built-in or declared via Spec.Models)
// are exposed.
func buildProvider(ctx context.Context, settings ResourceSettings, secrets *resource.SecretResolver) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(ctx, settings.Spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	profiles := make(map[string]profileMaterial, len(settings.Profiles))
	for _, profile := range settings.Profiles {
		material, err := newProfileMaterial(ctx, profile, secrets)
		if err != nil {
			return inference.ProviderDefinition{}, err
		}
		profiles[profile.ID] = material
	}

	provider := inference.ProviderDefinition{
		ID: settings.ID,
		ExtensionDecoders: map[string]inference.ExtensionDecoder{
			extensionGenerate: inference.ExtensionDecoderFor(func() *GenerateOptions {
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
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry := models[name]
		entry.api = spec.apiMode()
		id := inference.ModelID{Provider: settings.ID, Name: name}
		descriptor := inference.ModelDescriptor{
			ID:           id,
			Capabilities: entry.capabilities,
		}
		if entry.deprecated {
			descriptor.Lifecycle.Status = inference.ModelStatusDeprecated
			if entry.replacement != "" {
				replacement := inference.ModelID{
					Provider: settings.ID,
					Name:     entry.replacement,
				}
				descriptor.Lifecycle.Replacement = &replacement
			}
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

// openersFor binds one catalog model to the operation openers its kind
// serves. Each opener resolves the credential profile from ModelRef.Profile,
// builds service clients for it, and returns the driver set for the model's
// operation family. Transcription/realtime kinds are intentionally absent
// until core/inference exposes those operation surfaces.
func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	// The runtime validates ModelRef.Profile against the registered profiles
	// before any opener runs, so an unknown profile here is a provider bug.
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"openai model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(ctx, spec)
	}
	switch entry.kind {
	case kindGenerate:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openGenerate(cls, entry, id, model.Profile)
			},
		}
	case kindEmbed:
		return inference.Openers{
			Embed: func(
				ctx context.Context,
				model inference.ModelRef,
			) (inference.EmbedDriver, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return nil, err
				}
				return openEmbed(cls, entry, id, model.Profile)
			},
		}
	case kindImage:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openImage(cls, id, model.Profile)
			},
		}
	case kindTTS:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openTTS(cls, id, model.Profile)
			},
		}
	}
	return inference.Openers{}
}
