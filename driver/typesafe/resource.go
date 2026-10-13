package typesafe

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

// ResourceKind is the deployment resource kind implemented by the TypeSafe
// provider driver.
const ResourceKind = "inference.Provider"

// providerID labels this driver in compile rejections, provider logs, and
// spans. It is the driver's own name, not the deployment's provider id: a
// deployment may register this driver under any id it likes.
const providerID = "typesafe"

// ResourceSettings is the settings subtree of one TypeSafe provider resource:
// the provider identity, the credential-free spec, and one entry per credential
// profile. Secret values may carry ${env:NAME} references, resolved by the
// deployment loader before the driver sees them.
type ResourceSettings struct {
	// ID is the stable provider identity used by model refs and the
	// inference assembly (e.g. "typesafe").
	ID string `json:"id"`
	// Spec is the provider-owned, credential-free configuration.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Profiles declares one credential profile per API key.
	Profiles []ProfileSettings `json:"profiles,omitempty"`
}

// ProfileSettings is one credential profile. Secrets maps the provider-owned
// secret name (api_key) to a resolved or ${env:NAME} referenced value.
type ProfileSettings struct {
	ID         string                     `json:"id,omitempty"`
	Operations []model.Operation          `json:"operations,omitempty"`
	Secrets    map[string]resource.Secret `json:"secrets,omitempty"`
	Spec       json.RawMessage            `json:"spec,omitempty"`
}

type deployFactory struct{}

// Factory returns the TypeSafe provider deployment factory.
func Factory() resource.Factory {
	return deployFactory{}
}

// Spec implements resource.Factory.
func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: providerID}
}

// New implements resource.Factory: it strictly decodes the provider settings,
// resolves ${env:...} secret references, and builds the immutable
// core/inference.ProviderDefinition.
func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("typesafe provider: decode settings: %w", err)
	}
	if settings.ID == "" {
		return nil, fmt.Errorf("typesafe provider: settings.id is required")
	}
	return buildProvider(ctx, settings, in.Secrets)
}

// Register adds the TypeSafe provider factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}

// modelKind classifies a declared model by the family that serves it. It is an
// implementation discriminator — which wire compiler to bind — not a capability
// declaration: the content kinds a model serves are declared explicitly in its
// capabilities and validated against the family contract in
// validateDeclaration.
type modelKind string

// kindDecision is the only kind this driver serves: the System One route
// answers questions about one state value and produces no content kind, so
// there is no second family to dispatch on.
const kindDecision modelKind = "decision"

// buildProvider builds the provider definition from one deployment provider
// config. It validates the provider Spec, resolves every credential profile,
// and binds each declared model to the opener its kind serves. Unknown models
// fail closed: only models the deployment declares are exposed.
//
// No extension decoder is registered: this driver ships no provider extension,
// so a request that carries one has nothing to decode into and the compiler
// refuses it through the ledger.
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
	declaredProfiles := make(map[string]struct{}, len(settings.Profiles))
	for _, profile := range settings.Profiles {
		if profile.ID == "" {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"typesafe provider: a profile is missing its id")
		}
		// Two profiles under one id would leave the second one's api_key
		// silently in force and the first one's unreachable: the map below
		// would keep the last, and the definition would list the id twice.
		// A refusal names the collision instead.
		if _, duplicate := declaredProfiles[profile.ID]; duplicate {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"typesafe provider: duplicate profile %q", profile.ID)
		}
		declaredProfiles[profile.ID] = struct{}{}
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
		if err := validateDeclaration(modelSpec); err != nil {
			return inference.ProviderDefinition{}, fmt.Errorf(
				"model %q: %w", modelSpec.Name, err)
		}
		id := model.ModelID{Provider: settings.ID, Name: modelSpec.Name}
		provider.Models = append(provider.Models, inference.ModelImplementation{
			Descriptor: model.ModelDescriptor{
				ID:           id,
				Capabilities: modelSpec.Capabilities.Clone(),
				Limits:       modelSpec.Limits.Clone(),
			},
			Openers: openersFor(spec, profiles, id),
		})
	}
	return provider, nil
}

// openersFor binds one declared model to the openers its kind serves. A
// declaration of another kind never reaches here: ModelSpec.Validate refused it
// while the Spec was decoded.
func openersFor(
	spec Spec,
	profiles map[string]profileMaterial,
	id model.ModelID,
) inference.Openers {
	open := func(ctx context.Context, profile string) (*clients, error) {
		material, exists := profiles[profile]
		if !exists {
			return nil, fmt.Errorf(
				"typesafe: model %q references undeclared profile %q",
				id.Name,
				profile,
			)
		}
		return material.newClients(ctx, spec)
	}
	return inference.Openers{
		Decision: func(
			ctx context.Context,
			ref model.ModelRef,
		) (inference.DecisionDriver, error) {
			// The runtime resolves ref against the registered models before
			// any opener runs, so an unknown profile here is a provider bug
			// rather than a caller error.
			cls, err := open(ctx, ref.Profile)
			if err != nil {
				return nil, err
			}
			return openDecision(cls, id)
		},
	}
}
