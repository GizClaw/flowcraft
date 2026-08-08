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
	return sdkconfig.NewDocumentFactory(
		sdkconfig.ResourceSpec{
			Kind:     ResourceKind,
			Impl:     impl,
			ItemType: "memory.System",
			Deps:     deps,
		},
		func(ctx context.Context, data []byte, deps map[string]any) (any, error) {
			if factory == nil {
				return nil, errdefs.Validationf(
					"memory config: deploy factory has no implementation factory")
			}
			return factory.New(ctx, sdkmemory.Input{
				Settings: data,
				Deps:     deps,
			})
		},
	)
}
