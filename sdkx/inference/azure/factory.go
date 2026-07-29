package azure

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
)

// Factory builds the Azure OpenAI provider definition from one deployment
// provider config. Every declared deployment is exposed; there is no
// built-in catalog because the API does not report the backing model of a
// deployment. Operation drivers come from the openai package kernel.
func Factory() config.Factory {
	return config.FactoryFunc(func(
		_ context.Context,
		input config.ProviderInput,
	) (inference.ProviderDefinition, error) {
		spec, err := decodeSpec(input.Spec)
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
		for _, model := range spec.Models {
			id := inference.ModelID{Provider: input.ID, Name: model.Name}
			provider.Models = append(provider.Models, inference.ModelImplementation{
				Descriptor: inference.ModelDescriptor{ID: id},
				Openers:    openersFor(spec, model, profiles, id),
			})
		}
		return provider, nil
	})
}

// openersFor binds one deployment to the operation openers its kind serves,
// reusing the openai kernel drivers with the profile's Azure client.
func openersFor(
	spec Spec,
	model ModelSpec,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	// The runtime validates ModelRef.Profile against the registered profiles
	// before any opener runs, so an unknown profile here is a provider bug.
	open := func(profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"azure model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(spec), nil
	}
	caps := openai.Capabilities{
		Vision:     model.Vision,
		Reasoning:  model.Reasoning,
		Dimensions: model.Dimensions,
	}
	switch modelKind(model.Kind) {
	case kindGenerate:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openai.KernelGenerate(cls.api, id.Name, caps)
			},
		}
	case kindEmbed:
		return inference.Openers{
			Embed: func(
				_ context.Context,
				ref inference.ModelRef,
			) (inference.EmbedDriver, error) {
				cls, err := open(ref.Profile)
				if err != nil {
					return nil, err
				}
				return openai.KernelEmbed(cls.api, id.Name, caps)
			},
		}
	case kindImage:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openai.KernelImage(cls.api, id.Name)
			},
		}
	case kindTTS:
		return inference.Openers{
			Generate: func(
				_ context.Context,
				ref inference.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ref.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openai.KernelTTS(cls.api, id.Name)
			},
		}
	case kindASR:
		return inference.Openers{
			Transcription: func(
				_ context.Context,
				ref inference.ModelRef,
			) (inference.TranscriptionOperations, error) {
				cls, err := open(ref.Profile)
				if err != nil {
					return inference.TranscriptionOperations{}, err
				}
				return openai.KernelTranscription(cls.api, id.Name)
			},
		}
	}
	return inference.Openers{}
}
