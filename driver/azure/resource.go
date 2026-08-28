package azure

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the Azure
// provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one Azure provider resource.
type ResourceSettings struct {
	ID       string            `json:"id"`
	Spec     json.RawMessage   `json:"spec,omitempty"`
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile.
type ProfileSettings struct {
	ID         string                     `json:"id,omitempty"`
	Operations []inference.Operation      `json:"operations,omitempty"`
	Secrets    map[string]resource.Secret `json:"secrets,omitempty"`
	Spec       json.RawMessage            `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the Azure provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "azure"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("azure provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("azure provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the Azure provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

func buildProvider(ctx context.Context, settings ResourceSettings, secrets *resource.SecretResolver) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(ctx, settings.Spec)
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
			extensionImage: inference.ExtensionDecoderFor(func() *ImageOptions {
				return &ImageOptions{Provider: settings.ID}
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
	for _, model := range spec.Models {
		id := inference.ModelID{Provider: settings.ID, Name: model.Name}
		entry := entryFor(model)
		provider.Models = append(provider.Models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID:           id,
				Capabilities: entry.capabilities,
			},
			Openers: openersFor(spec, entry, profiles, id),
		})
	}
	return provider, nil
}

// openersFor binds one deployment to the operation openers its kind serves,
// through the self-hosted kernel drivers with the profile's Azure client.
func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"azure model %s references undeclared profile %q",
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
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openGenerate(cls, entry, id, ref.Profile)
			},
		}
	case kindEmbed:
		return inference.Openers{
			Embed: func(
				ctx context.Context,
				ref inference.ModelRef,
			) (inference.EmbedDriver, error) {
				cls, err := open(ctx, ref.Profile)
				if err != nil {
					return nil, err
				}
				return openEmbed(cls, entry, id, ref.Profile)
			},
		}
	case kindImage:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openImage(cls, id, ref.Profile)
			},
		}
	case kindTTS:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openTTS(cls, id, ref.Profile)
			},
		}
	}
	return inference.Openers{}
}
