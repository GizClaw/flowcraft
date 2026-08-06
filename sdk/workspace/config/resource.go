package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ResourceKind is the deployment resource category this package builds.
// A dep bound to the whole resource must declare this as its Type; a dep
// spelled "name/item" binds one workspace.Workspace out of it, because
// [Registry] is a container (see [Registry.ResolveItem]).
const ResourceKind = "workspace.Registry"

// ResourceSettings is the settings subtree of a workspace resource.
// It only says where the sub-document lives; relative local-driver
// roots resolve against the host builder's Deps.BaseDir, which the
// document does not restate.
type ResourceSettings struct {
	config.SubDocument
}

type deployFactory struct {
	builder *Builder
}

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
	return deployFactory{builder: builder}
}

func (deployFactory) Spec() config.ResourceSpec {
	return config.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "workspace.Workspace",
	}
}

// New builds a workspace [Registry] owned by the deployment result.
func (f deployFactory) New(ctx context.Context, in config.Input) (any, error) {
	if f.builder == nil {
		return nil, errdefs.Validationf(
			"workspace config: deploy factory builder is nil")
	}
	var settings ResourceSettings
	if err := in.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace config: decode resource settings: %w", err))
	}
	data, err := settings.Bytes()
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return f.builder.Build(ctx, doc)
}
