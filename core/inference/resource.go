package inference

import (
	"context"

	"github.com/GizClaw/flowcraft/core/resource"
)

// depProvider is the provider set this assembly is built from: every dep key
// equal to "provider" or starting with "provider." (the driver modules
// register one value each).
var depProvider = resource.RequireMany[ProviderDefinition]("provider")

// Factory builds an inference Assembly from its many provider deps.
type Factory struct{}

// Spec implements resource.Factory. The provider set is the only dependency:
// one [ProviderDefinition] value per registered provider resource.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "inference.Assembly",
		Impl: "unified",
		Deps: []resource.Requirement{depProvider.Spec()},
	}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (*Assembly, error) {
	values, err := depProvider.GetAll(in)
	if err != nil {
		return nil, err
	}
	providers := make(map[string]ProviderDefinition, len(values))
	for _, def := range values {
		providers[def.ID] = def
	}
	return &Assembly{providers: providers}, nil
}

// Register adds the assembly factory to the registry.
func Register(r *resource.Registry) error {
	return resource.Register(r, Factory{})
}
