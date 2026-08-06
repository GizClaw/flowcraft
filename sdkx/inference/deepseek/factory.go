package deepseek

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// Factory builds the DeepSeek provider definition from the deployment
// configuration: one catalog of models, one set of profiles, openers that
// bind each model to its drivers. Wire it into the deployment config like:
//
//	builder := config.NewBuilder(
//		map[string]config.Factory{"deepseek": deepseek.Factory()},
//		resolvers,
//	)
func Factory() config.Factory {
	return config.FactoryFunc(func(_ context.Context, input config.ProviderInput) (inference.ProviderDefinition, error) {
		spec, err := decodeSpec(input.Spec)
		if err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf("deepseek provider %q: %w", input.ID, err)
		}
		models, err := mergedCatalog(spec)
		if err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf("deepseek provider %q: %w", input.ID, err)
		}

		profiles := make(map[string]profileMaterial, len(input.Profiles))
		for _, profile := range input.Profiles {
			material, err := newProfileMaterial(profile)
			if err != nil {
				return inference.ProviderDefinition{}, fmt.Errorf("deepseek provider %q: %w", input.ID, err)
			}
			profiles[profile.ID] = material
		}

		provider := inference.ProviderDefinition{ID: input.ID}
		for _, profile := range input.Profiles {
			provider.Profiles = append(provider.Profiles, inference.ProfileDefinition{
				ID:         profile.ID,
				Operations: append([]inference.Operation(nil), profile.Operations...),
			})
		}
		for _, name := range sortedNames(models) {
			entry := models[name]
			id := inference.ModelID{Provider: input.ID, Name: name}
			provider.Models = append(provider.Models, inference.ModelImplementation{
				Descriptor: inference.ModelDescriptor{ID: id},
				Openers:    openersFor(spec, entry, profiles, id),
			})
		}
		return provider, nil
	})
}

func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	open := func(profile string) (*clients, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf("deepseek: model %q references undeclared profile %q", id.Name, profile)
		}
		return material.newClients(spec), nil
	}

	var openers inference.Openers
	if entry.kind == kindGenerate {
		openers.Generate = func(_ context.Context, model inference.ModelRef) (inference.GenerateOperations, error) {
			cls, err := open(model.Profile)
			if err != nil {
				return inference.GenerateOperations{}, err
			}
			return openGenerate(cls, entry, id, model.Profile)
		}
	}
	return openers
}
