package minimax

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// MiniMax provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one MiniMax provider
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

// Factory returns the MiniMax provider deployment factory.
func Factory() resource.Factory { return deployFactory{} }

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "minimax"}
}

// New implements resource.Factory.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](
		ctx, in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, fmt.Errorf("minimax provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("minimax provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the MiniMax provider factory to r.
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

	provider := inference.ProviderDefinition{
		ID: settings.ID,
		ExtensionDecoders: map[string]inference.ExtensionDecoder{
			extensionMusic: inference.ExtensionDecoderFor(func() *MusicOptions {
				return &MusicOptions{Provider: settings.ID}
			}),
			extensionVideo: inference.ExtensionDecoderFor(func() *VideoOptions {
				return &VideoOptions{Provider: settings.ID}
			}),
			extensionContextIR: inference.ExtensionDecoderFor(func() *ContextIROptions {
				return &ContextIROptions{Provider: settings.ID}
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
		descriptor := inference.ModelDescriptor{
			ID:           id,
			Capabilities: entry.capabilities,
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

// openersFor binds one catalog model to the openers its kind serves.
func openersFor(
	spec Spec,
	entry catalogEntry,
	profiles map[string]profileMaterial,
	id inference.ModelID,
) inference.Openers {
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf("minimax: model %q references undeclared profile %q", id.Name, profile)
		}
		return material.newClients(ctx, spec)
	}

	var openers inference.Openers
	openers.Generate = func(ctx context.Context, model inference.ModelRef) (inference.GenerateOperations, error) {
		cls, err := open(ctx, model.Profile)
		if err != nil {
			return inference.GenerateOperations{}, err
		}
		switch entry.kind {
		case kindGenerate:
			return openGenerate(cls, entry, id, model.Profile)
		case kindImage:
			return openImage(cls, entry, id)
		case kindTTS:
			return openTTS(cls, entry, id)
		case kindVideo:
			return openVideo(cls, spec, entry, id)
		case kindContextIR:
			return openContextIR(cls, spec, entry, id)
		case kindMusic:
			return openMusic(cls, entry, id)
		default:
			return inference.GenerateOperations{}, fmt.Errorf(
				"minimax: model %q has unsupported kind %q", id.Name, entry.kind,
			)
		}
	}
	return openers
}
