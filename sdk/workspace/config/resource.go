package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ResourceKind is the deployment resource category this package builds.
// A dep bound to the whole resource must declare this as its Type; a dep
// spelled "name/item" binds one workspace.Workspace out of it, because
// [Registry] is a container (see [Registry.ResolveItem]).
const ResourceKind = "workspace.Registry"

// NewDeployFactory returns the deployment factory for workspace
// registries over the host's builder. Passing a builder is what lets a
// document use drivers the host registered (object-store backends,
// custom local roots, ...) — the deployment engine only sees the
// resource factory, never the driver catalog.
//
// Registration is opt-in from the host so that a deployment engine
// never imports this package:
//
//	wb := NewBuilder(Deps{BaseDir: dir})
//	wb.RegisterFactory("objstore.s3", s3Factory)
//	builder.RegisterResource(NewDeployFactory(wb))
func NewDeployFactory(builder *Builder) config.ResourceFactory {
	return config.NewDocumentFactory(
		config.ResourceSpec{
			Kind:     ResourceKind,
			Impl:     "yaml",
			ItemType: "workspace.Workspace",
		},
		func(ctx context.Context, data []byte, deps map[string]any) (any, error) {
			if builder == nil {
				return nil, errdefs.Validationf(
					"workspace config: deploy factory builder is nil")
			}
			doc, err := Parse(data)
			if err != nil {
				return nil, err
			}
			return builder.Build(ctx, doc)
		},
	)
}
