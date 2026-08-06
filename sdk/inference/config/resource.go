package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ResourceKind is the deployment resource category this package builds.
// The value bound is a *Assembly, holding the Runtime and the optional
// Router: graph's inference node needs both, and a route policy is
// meaningless once separated from the runtime it validated against.
const ResourceKind = "inference.Assembly"

// ResourceSettings is the settings subtree of an inference resource.
type ResourceSettings struct {
	config.SubDocument
}

type deployFactory struct {
	factories map[string]Factory
	resolvers map[string]SecretResolver
}

// NewDeployFactory returns the deployment factory for inference
// assemblies.
//
// Provider factories and secret resolvers are Go values that the
// document cannot name. A deployment declares WHICH providers it wants
// and where their credentials live; the host decides which driver code
// and which secret backends exist in the binary. That split is what
// keeps credentials out of the document — profiles carry
// [SecretRef], never values.
func NewDeployFactory(
	factories map[string]Factory,
	resolvers map[string]SecretResolver,
) config.ResourceFactory {
	return &deployFactory{factories: factories, resolvers: resolvers}
}

func (*deployFactory) Spec() config.ResourceSpec {
	return config.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "inference.Runtime",
	}
}

func (f *deployFactory) New(ctx context.Context, in config.Input) (any, error) {
	var settings ResourceSettings
	if err := in.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"inference config: decode resource settings: %w", err))
	}
	data, err := settings.Bytes()
	if err != nil {
		return nil, err
	}
	document, err := Parse(data)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	builder, err := NewBuilder(f.factories, f.resolvers)
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
