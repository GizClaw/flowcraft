package anthropic

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
// Anthropic provider driver.
const ResourceKind = "inference.Provider"

// providerID is this driver's provider identity: it labels the compile errors
// the ledger builds and tags telemetry, matching the model.ModelID.Provider
// values the deployment's declarations publish.
const providerID = "anthropic"

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
	Operations []model.Operation          `json:"operations,omitempty"`
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
	wire := spec.dialect()
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
			Openers: openersFor(spec, modelSpec, wire, profiles, id),
		})
	}
	return provider, nil
}

// validateModel enforces the family contract: the Messages compiler serves
// text output, so a declaration that promises anything else is a build error
// rather than a per-request rejection.
func validateModel(declared ModelSpec) error {
	if err := declared.Capabilities.Validate(); err != nil {
		return err
	}
	if !slices.Contains(declared.Capabilities.Outputs, message.PartText) {
		return fmt.Errorf("generate family must declare text output")
	}
	return declared.Limits.Validate()
}

// openersFor binds one declared model to the generate openers.
func openersFor(
	spec Spec,
	declared ModelSpec,
	wire dialect,
	profiles map[string]profileMaterial,
	id model.ModelID,
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
			model model.ModelRef,
		) (inference.GenerateOperations, error) {
			cls, err := open(ctx, model.Profile)
			if err != nil {
				return inference.GenerateOperations{}, err
			}
			return openGenerate(cls, declared, wire, id, model.Profile)
		},
	}
}
