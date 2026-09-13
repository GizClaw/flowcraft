package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by the
// OpenAI provider driver.
const ResourceKind = "inference.Provider"

// ResourceSettings is the settings subtree of one OpenAI provider
// resource: the provider identity, credential-free spec, and one entry
// per credential profile. Secret values may carry ${env:NAME}
// references, resolved by the driver at build time.
type ResourceSettings struct {
	// ID is the stable provider identity used by model refs and the
	// inference assembly (e.g. "openai").
	ID string `json:"id"`
	// Spec is the provider-owned, credential-free configuration.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Profiles declares one credential profile per API key/account.
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile. Secrets maps the
// provider-owned secret name (api_key) to a resolved or ${env:NAME}
// referenced value.
type ProfileSettings struct {
	ID         string                     `json:"id,omitempty"`
	Operations []model.Operation          `json:"operations,omitempty"`
	Secrets    map[string]resource.Secret `json:"secrets,omitempty"`
	Spec       json.RawMessage            `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the OpenAI provider deployment factory.
func Factory() resource.Factory {
	return deployFactory{}
}

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "openai"}
}

// New implements resource.Factory: it strictly decodes the provider
// settings, resolves ${env:...} secret references, and builds the
// immutable core/inference.ProviderDefinition.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("openai provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("openai provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the OpenAI provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

// modelKind classifies a declared model by the compiler family that serves it.
// It is an implementation discriminator — which wire compiler to bind — not a
// capability declaration: the content kinds a model serves are declared
// explicitly in its capabilities and validated against the family contract in
// validateModel.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
)

// buildProvider builds the OpenAI provider definition from one
// deployment provider config. It validates the provider Spec, resolves
// every credential profile, and binds each declared model to the openers its
// kind serves. Unknown models fail closed: only models the deployment
// declares are exposed.
func buildProvider(ctx context.Context, settings ResourceSettings, secrets *resource.SecretResolver) (inference.ProviderDefinition, error) {
	spec, err := decodeSpec(ctx, settings.Spec)
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	wire := spec.dialect()
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
			extensionTTS: inference.ExtensionDecoderFor(func() *TTSOptions {
				return &TTSOptions{Provider: settings.ID}
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
		kind := modelKind(modelSpec.Kind)
		if err := validateModel(kind, modelSpec, wire); err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"model %q: %w", modelSpec.Name, err)
		}
		modelSpec = wire.narrowModel(kind, modelSpec)
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
			Openers: openersFor(spec, wire, kind, modelSpec, profiles, id),
		})
	}
	return provider, nil
}

// validateModel enforces the family contract: the compiler bound by kind can
// only serve the output modalities it produces, and a declared input kind must
// be one this wire can carry. It runs once per model at build time, before any
// request reaches the compiler.
func validateModel(kind modelKind, declared ModelSpec, wire dialect) error {
	if err := declared.Capabilities.Validate(); err != nil {
		return err
	}
	for _, input := range declared.Capabilities.Inputs {
		switch input {
		case message.PartAudio:
			return fmt.Errorf(
				"the OpenAI wire has no audio input; drop it from capabilities.inputs",
			)
		case message.PartVideo:
			// Video is a compatible-endpoint extension: the family carries it
			// only when the deployment states the endpoint fact (and the spec
			// requires that fact to be on the chat surface, the one with a
			// lowering).
			if !wire.video {
				return fmt.Errorf(
					"the endpoint does not accept video input; " +
						"set spec.wire.video_input on a chat-surface deployment, " +
						"or drop it from capabilities.inputs",
				)
			}
		case message.PartFile:
			return fmt.Errorf(
				"the OpenAI wire has no file input; drop it from capabilities.inputs",
			)
		}
	}
	switch kind {
	case kindGenerate:
		if !slices.Contains(declared.Capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !slices.Contains(declared.Capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindTTS:
		if !slices.Contains(declared.Capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("tts family must declare audio output")
		}
	case kindEmbed:
		if len(declared.Capabilities.Outputs) != 0 {
			return fmt.Errorf("embed family declares no generate output")
		}
	}
	return declared.Limits.Validate()
}

// openersFor binds one declared model to the operation openers its kind
// serves. Each opener resolves the credential profile from ModelRef.Profile,
// builds service clients for it, and returns the driver set for the model's
// operation family. Transcription/realtime kinds are intentionally absent
// until core/inference exposes those operation surfaces.
func openersFor(
	spec Spec,
	wire dialect,
	kind modelKind,
	declared ModelSpec,
	profiles map[string]profileMaterial,
	id model.ModelID,
) inference.Openers {
	// The runtime validates ModelRef.Profile against the registered profiles
	// before any opener runs, so an unknown profile here is a provider bug.
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf(
				"openai model %s references undeclared profile %q",
				id,
				profile,
			)
		}
		return material.newClients(ctx, spec)
	}
	switch kind {
	case kindGenerate:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model model.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openGenerate(cls, declared, wire, id, model.Profile)
			},
		}
	case kindEmbed:
		return inference.Openers{
			Embed: func(
				ctx context.Context,
				model model.ModelRef,
			) (inference.EmbedDriver, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return nil, err
				}
				return openEmbed(cls, declared, wire, id, model.Profile)
			},
		}
	case kindImage:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model model.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openImage(cls, declared, wire, id, model.Profile)
			},
		}
	case kindTTS:
		return inference.Openers{
			Generate: func(
				ctx context.Context,
				model model.ModelRef,
			) (inference.GenerateOperations, error) {
				cls, err := open(ctx, model.Profile)
				if err != nil {
					return inference.GenerateOperations{}, err
				}
				return openTTS(cls, id, model.Profile)
			},
		}
	}
	return inference.Openers{}
}
