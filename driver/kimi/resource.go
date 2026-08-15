package kimi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the Kimi
// provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one Kimi provider resource.
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

// Factory returns the Kimi provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "kimi"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](
		in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, fmt.Errorf("kimi provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("kimi provider: settings.id is required")
	}
	return buildProvider(settings)
}

// Register adds the Kimi provider factory to r.
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
		id := inference.ModelID{Provider: settings.ID, Name: name}
		provider.Models = append(provider.Models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{ID: id},
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
	open := func(profile string) (*kimiClient, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf("kimi: model %q references undeclared profile %q", id.Name, profile)
		}
		return material.newClient(spec), nil
	}

	var openers inference.Openers
	if entry.kind == kindGenerate {
		openers.Generate = func(_ context.Context, model inference.ModelRef) (inference.GenerateOperations, error) {
			client, err := open(model.Profile)
			if err != nil {
				return inference.GenerateOperations{}, err
			}
			return inference.BindGenerateOperations(
				compileGenerate(id.Name, entry),
				transportGenerate(client),
				decodeGenerate,
				transportGenerateStream(client),
				decodeGenerateStream,
			)
		}
	}
	return openers
}
