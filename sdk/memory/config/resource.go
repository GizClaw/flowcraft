package config

import (
	"context"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// ResourceKind is the deployment resource category for memory
// assemblies.
const ResourceKind = "memory.Assembly"

type deployFactory struct {
	impl    string
	factory sdkmemory.Factory
	deps    []sdkconfig.ResourceDepSpec
}

// NewDeployFactory returns a deployment factory for one memory
// implementation. The implementation is addressed in documents as the
// resource's impl, e.g.:
//
//	resources:
//	  memories:
//	    kind: memory.Assembly
//	    impl: flowcraft
//	    settings: {file: ./memory.yaml}
//
// Deps declares the resource dependencies the implementation needs; the
// sdk/memory protocol itself never hard-codes them. The flowcraft
// implementation, for example, registers "inference" and "workspace"
// here. Every bound dependency is forwarded to the implementation
// factory, which names and type-asserts its own dependencies.
func NewDeployFactory(
	impl string,
	factory sdkmemory.Factory,
	deps ...sdkconfig.ResourceDepSpec,
) sdkconfig.ResourceFactory {
	return &deployFactory{
		impl:    impl,
		factory: factory,
		deps:    append([]sdkconfig.ResourceDepSpec(nil), deps...),
	}
}

func (f *deployFactory) Spec() sdkconfig.ResourceSpec {
	return sdkconfig.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     f.impl,
		ItemType: "memory.System",
		Deps:     f.deps,
	}
}

func (f *deployFactory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	if f.factory == nil {
		return nil, errdefs.Validationf(
			"memory config: deploy factory has no implementation factory")
	}
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	return f.factory.New(ctx, sdkmemory.Input{
		Settings: data,
		Deps:     in.Deps,
	})
}
