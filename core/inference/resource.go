package inference

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Factory builds an inference Assembly from its many provider deps.
// Each dep must be a [ProviderDefinition] (or a pointer to one) —
// that is the provider resource value.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "inference.Assembly",
		Impl: "unified",
		Deps: []resource.DepSpec{{
			Name: "provider", Type: "inference.Provider", Required: true, Many: true,
		}},
	}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	providers := make(map[string]ProviderDefinition, len(in.Deps))
	for _, value := range in.DepsMany("provider") {
		var def ProviderDefinition
		switch v := value.(type) {
		case ProviderDefinition:
			def = v
		case *ProviderDefinition:
			def = *v
		default:
			return nil, errdefs.Validationf(
				"inference: provider dep is not a ProviderDefinition")
		}
		providers[def.ID] = def
	}
	if len(providers) == 0 {
		return nil, errdefs.Validationf(
			"inference: assembly requires at least one provider")
	}
	return &Assembly{providers: providers}, nil
}

// Register adds the assembly factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
