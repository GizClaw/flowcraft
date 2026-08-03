package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ResourceKind is the deploy resource category this package builds. A
// dep bound to the whole resource must declare this as its Type; a dep
// spelled "name/item" binds one workspace.Workspace out of it, because
// [Registry] is a container (see [Registry.ResolveItem]).
const ResourceKind = "workspace.Registry"

// ResourceSettings is the settings subtree of a workspace resource.
type ResourceSettings struct {
	// BaseDir resolves relative local-driver roots. It is deliberately
	// separate from SubDocument.File: File is just a path to read once,
	// while BaseDir is workspace path semantics that outlive loading.
	BaseDir string `yaml:"base_dir,omitempty"`

	deploy.SubDocument `yaml:",inline"`
}

type deployFactory struct{}

// NewDeployFactory returns the YAML deploy factory for workspace registries.
//
// Registration is opt-in from the host so that sdkx/deploy never
// imports this package:
//
//	b.RegisterResource(config.NewDeployFactory())
func NewDeployFactory() deploy.ResourceFactory {
	return deployFactory{}
}

func (deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "workspace.Workspace",
	}
}

// New builds a workspace [Registry] owned by the deployment result.
func (deployFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace config: decode resource settings: %w", err))
	}
	data, err := settings.YAML()
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return NewBuilder(Deps{BaseDir: settings.BaseDir}).Build(ctx, doc)
}
