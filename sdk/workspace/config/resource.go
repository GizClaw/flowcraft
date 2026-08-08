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

// Spec implements config.Factory.
func (b *Builder) Spec() config.Spec {
	return config.Spec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "workspace.Workspace",
	}
}

// New implements config.Factory: the settings subtree is the workspace
// document, resolved through the input's shared loader and built over
// the host builder (which holds the registered driver catalog).
func (b *Builder) New(ctx context.Context, in config.Input) (any, error) {
	if b == nil {
		return nil, errdefs.Validationf(
			"workspace config: builder is nil")
	}
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return b.Build(ctx, doc)
}
