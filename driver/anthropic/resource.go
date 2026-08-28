package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// Anthropic provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one Anthropic provider
// resource.
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

// Factory returns the Anthropic provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "anthropic"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("anthropic provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("anthropic provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the Anthropic provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

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

	provider := inference.ProviderDefinition{ID: settings.ID}
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

// openersFor binds one catalog model to the generate openers.
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
				"anthropic model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(ctx, spec)
	}
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
}
