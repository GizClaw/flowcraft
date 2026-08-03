package yaml

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// ResourceKind is the deploy resource category this package builds.
// The value bound is a *config.Assembly, holding the Runtime and the
// optional Router: graph's inference node needs both, and a route
// policy is meaningless once separated from the runtime it validated
// against.
const ResourceKind = "inference.Assembly"

// ResourceSettings is the settings subtree of an inference resource.
type ResourceSettings struct {
	deploy.SubDocument `yaml:",inline"`
}

type deployFactory struct {
	factories map[string]config.Factory
	resolvers map[string]config.SecretResolver
}

// NewDeployFactory returns the YAML deploy factory for inference assemblies.
//
// Provider factories and secret resolvers are Go values that YAML cannot
// name. A deployment declares WHICH providers
// it wants and where their credentials live; the host decides which
// driver code and which secret backends exist in the binary. That
// split is what keeps credentials out of the document — profiles carry
// [config.SecretRef], never values.
//
//	b.RegisterResource(yaml.NewDeployFactory(factories, resolvers))
func NewDeployFactory(
	factories map[string]config.Factory,
	resolvers map[string]config.SecretResolver,
) deploy.ResourceFactory {
	return &deployFactory{factories: factories, resolvers: resolvers}
}

func (*deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "inference.Runtime",
	}
}

func (f *deployFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"inference config: decode resource settings: %w", err))
	}
	data, err := settings.YAML()
	if err != nil {
		return nil, err
	}
	document, err := decode(data)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	builder, err := config.NewBuilder(f.factories, f.resolvers)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"inference config: %w", err))
	}
	assembly, err := builder.NewAssembly(ctx, document)
	if err != nil {
		return nil, err
	}
	// Return a pointer: Assembly is a two-field value, and every consumer
	// must observe the same Runtime rather than a copy of its wrapper.
	return &assembly, nil
}
