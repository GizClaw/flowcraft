package bytedance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// ByteDance provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one ByteDance provider
// resource.
type ResourceSettings struct {
	ID       string            `json:"id"`
	Spec     json.RawMessage   `json:"spec,omitempty"`
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile.
type ProfileSettings struct {
	ID         string                `json:"id,omitempty"`
	Operations []inference.Operation `json:"operations,omitempty"`
	Secrets    map[string]string     `json:"secrets,omitempty"`
	Spec       json.RawMessage       `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the ByteDance provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "bytedance"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](
		in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, fmt.Errorf("bytedance provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("bytedance provider: settings.id is required")
	}
	return buildProvider(settings)
}

// Register adds the ByteDance provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

func buildProvider(settings ResourceSettings) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(settings.Spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
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
				return &GenerateOptions{Provider: settings.ID}
			}),
			extensionImage: inference.ExtensionDecoderFor(func() *ImageOptions {
				return &ImageOptions{Provider: settings.ID}
			}),
			extensionVideo: inference.ExtensionDecoderFor(func() *VideoOptions {
				return &VideoOptions{Provider: settings.ID}
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
// serves.
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
				"bytedance model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(spec), nil
	}
	switch entry.kind {
	case kindGenerate:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openGenerate(cls, spec, entry, id, model.Profile)
			},
		}
	case kindEmbed:
		return inference.Openers{
			Embed: func(
				_ context.Context,
				model inference.ModelRef,
			) (inference.EmbedDriver, error) {
				cls, err := open(model.Profile)
				if err != nil {
					return nil, err
				}
				return openEmbed(cls, spec, entry, id, model.Profile)
			},
		}
	case kindImage:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openImage(cls, spec, id, model.Profile)
			},
		}
	case kindVideo:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				model inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openVideo(cls, spec, entry, id, model.Profile)
			},
		}
	}
	return inference.Openers{}
}
