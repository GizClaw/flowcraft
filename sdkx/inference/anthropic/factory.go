package anthropic

import (
	"context"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// Factory builds the Anthropic provider definition from one deployment
// provider config. Every catalog model is generate kind — the Messages API
// is the whole Claude surface — so openers bind chat unary + stream only.
// Unknown models fail closed: only catalog models (built-in or declared via
// Spec.Models) are exposed.
func Factory() config.Factory {
	return config.FactoryFunc(func(
		_ context.Context,
		input config.ProviderInput,
	) (inference.ProviderDefinition, error) {
		spec, err := decodeSpec(input.Spec)
		if err != nil {
			return inference.ProviderDefinition{}, err
		}
		models, err := mergedCatalog(spec)
		if err != nil {
			return inference.ProviderDefinition{}, err
		}
		profiles := make(map[string]profileMaterial, len(input.Profiles))
		for _, profile := range input.Profiles {
			material, err := newProfileMaterial(profile)
			if err != nil {
				return inference.ProviderDefinition{}, err
			}
			profiles[profile.ID] = material
		}

		provider := inference.ProviderDefinition{ID: input.ID}
		for _, profile := range input.Profiles {
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
			id := inference.ModelID{Provider: input.ID, Name: name}
			descriptor := inference.ModelDescriptor{ID: id}
			if entry.deprecated {
				descriptor.Lifecycle.Status = inference.ModelStatusDeprecated
				if entry.replacement != "" {
					replacement := inference.ModelID{
						Provider: input.ID,
						Name:     entry.replacement,
					}
					descriptor.Lifecycle.Replacement = &replacement
				}
			}
			provider.Models = append(provider.Models, inference.ModelImplementation{
				Descriptor: descriptor,
				Openers:    openersFor(spec, entry, profiles, id),
			})
		}
		return provider, nil
	})
}

// openersFor binds one catalog model to the generate openers. Each opener
// resolves the credential profile from ModelRef.Profile, builds the service
// client for it, and returns the chat driver set.
func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	// The runtime validates ModelRef.Profile against the registered profiles
	// before any opener runs, so an unknown profile here is a provider bug.
	open := func(profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"anthropic model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(spec), nil
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
