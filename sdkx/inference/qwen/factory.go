package qwen

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// Factory builds the Qwen (DashScope) provider definition from the
// deployment configuration: one catalog of models, one set of profiles,
// openers that bind each model to its drivers. Wire it into the
// deployment config like:
//
//	builder := config.NewBuilder(
//		map[string]config.Factory{"qwen": qwen.Factory()},
//		resolvers,
//	)
func Factory() config.Factory {
	return config.FactoryFunc(func(_ context.Context, input config.ProviderInput) (inference.ProviderDefinition, error) {
		spec, err := decodeSpec(input.Spec)
		if err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf("qwen provider %q: %w", input.ID, err)
		}
		models, err := mergedCatalog(spec)
		if err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf("qwen provider %q: %w", input.ID, err)
		}

		profiles := make(map[string]profileMaterial, len(input.Profiles))
		for _, profile := range input.Profiles {
			material, err := newProfileMaterial(profile)
			if err != nil {
				return inference.ProviderDefinition{}, fmt.Errorf("qwen provider %q: %w", input.ID, err)
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

func sortedNames(models map[string]catalogEntry) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	open := func(profile string) (*dashClient, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf("qwen: model %q references undeclared profile %q", id.Name, profile)
		}
		return material.newClient(spec), nil
	}

	var openers inference.Openers
	switch entry.kind {
	case kindGenerate:
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
				decodeStreamFragment,
			)
		}
	case kindEmbed:
		openers.Embed = func(_ context.Context, model inference.ModelRef) (inference.EmbedDriver, error) {
			client, err := open(model.Profile)
			if err != nil {
				return nil, err
			}
			return inference.BindEmbed(
				compileEmbed(id.Name, entry),
				transportEmbed(client),
				decodeEmbed,
			)
		}
	}
	return openers
}
