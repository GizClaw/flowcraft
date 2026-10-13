package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the DashScope
// provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one DashScope provider
// resource.
type ResourceSettings struct {
	ID       string            `json:"id"`
	Spec     json.RawMessage   `json:"spec,omitempty"`
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile. DashScope keys are region-bound,
// so a deployment serving more than one region declares one profile per host.
type ProfileSettings struct {
	ID         string                     `json:"id,omitempty"`
	Operations []model.Operation          `json:"operations,omitempty"`
	Secrets    map[string]resource.Secret `json:"secrets,omitempty"`
	Spec       json.RawMessage            `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the DashScope provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "dashscope"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("dashscope provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("dashscope provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the DashScope provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

func buildProvider(
	ctx context.Context,
	settings ResourceSettings,
	secrets *resource.SecretResolver,
) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(ctx, settings.Spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	profiles := make(map[string]profileMaterial, len(settings.Profiles))
	for _, profile := range settings.Profiles {
		if profile.ID == "" {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"dashscope provider: a profile is missing its id")
		}
		material, err := newProfileMaterial(ctx, profile, secrets)
		if err != nil {
			return inference.ProviderDefinition{}, err
		}
		profiles[profile.ID] = material
	}

	provider := inference.ProviderDefinition{
		ID: settings.ID,
		ExtensionDecoders: map[string]inference.ExtensionDecoder{
			extensionImage: inference.ExtensionDecoderFor(func() *ImageOptions {
				return &ImageOptions{Provider: settings.ID}
			}),
			extensionVideo: inference.ExtensionDecoderFor(func() *VideoOptions {
				return &VideoOptions{Provider: settings.ID}
			}),
			extensionGenerate: inference.ExtensionDecoderFor(func() *GenerateOptions {
				return &GenerateOptions{Provider: settings.ID}
			}),
			extensionEmbed: inference.ExtensionDecoderFor(func() *EmbedOptions {
				return &EmbedOptions{Provider: settings.ID}
			}),
			extensionTranscribe: inference.ExtensionDecoderFor(func() *TranscribeOptions {
				return &TranscribeOptions{Provider: settings.ID}
			}),
		},
	}
	for _, profile := range settings.Profiles {
		provider.Profiles = append(
			provider.Profiles,
			inference.ProfileDefinition{
				ID:         profile.ID,
				Operations: append([]model.Operation(nil), profile.Operations...),
			},
		)
	}
	declared := append([]ModelSpec(nil), spec.Models...)
	slices.SortFunc(declared, func(left, right ModelSpec) int {
		return strings.Compare(left.Name, right.Name)
	})
	for _, modelSpec := range declared {
		if err := validateModel(modelSpec); err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"model %q: %w", modelSpec.Name, err)
		}
		id := model.ModelID{Provider: settings.ID, Name: modelSpec.Name}
		if err := modelSpec.Lifecycle.ValidateFor(id); err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"model %q: %w", modelSpec.Name, err)
		}
		provider.Models = append(provider.Models, inference.ModelImplementation{
			Descriptor: model.ModelDescriptor{
				ID:           id,
				Capabilities: modelSpec.Capabilities.Clone(),
				Limits:       modelSpec.Limits.Clone(),
				Lifecycle:    modelSpec.Lifecycle.Clone(),
			},
			Openers: openersFor(spec, modelSpec, profiles, id),
		})
	}
	return provider, nil
}

// openersFor binds one declared model to the openers its kind serves.
func openersFor(
	spec Spec,
	declared ModelSpec,
	profiles map[string]profileMaterial,
	id model.ModelID,
) inference.Openers {
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf(
				"dashscope: model %q references undeclared profile %q", id.Name, profile)
		}
		return material.newClients(ctx, spec)
	}

	var openers inference.Openers
	switch modelKind(declared.Kind) {
	case kindImage, kindVideo, kindGenerate, kindTTS:
		openers.Generate = func(
			ctx context.Context,
			model model.ModelRef,
		) (inference.GenerateOperations, error) {
			cls, err := open(ctx, model.Profile)
			if err != nil {
				return inference.GenerateOperations{}, err
			}
			switch modelKind(declared.Kind) {
			case kindImage:
				return openImage(cls, spec, declared, id)
			case kindVideo:
				return openVideo(cls, spec, declared, id)
			case kindTTS:
				return openTTS(cls, declared, id)
			default:
				return openGenerate(cls, spec, declared, id, model.Profile)
			}
		}
	case kindEmbed:
		openers.Embed = func(
			ctx context.Context,
			model model.ModelRef,
		) (inference.EmbedDriver, error) {
			cls, err := open(ctx, model.Profile)
			if err != nil {
				return nil, err
			}
			return openEmbed(cls, declared, id)
		}
	case kindTranscribe:
		openers.Transcribe = func(
			ctx context.Context,
			model model.ModelRef,
		) (inference.TranscribeOperations, error) {
			cls, err := open(ctx, model.Profile)
			if err != nil {
				return inference.TranscribeOperations{}, err
			}
			return openTranscribe(cls, spec, declared, id)
		}
	}
	return openers
}
