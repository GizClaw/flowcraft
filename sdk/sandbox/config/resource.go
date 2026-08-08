package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
)

// ResourceKind is the deployment resource category this package builds.
// A dep bound to the whole resource must declare this as its Type; a dep
// spelled "name/item" binds one sandbox.Runner out of it, because
// [Registry] is a container (see [Registry.ResolveItem]).
const ResourceKind = "sandbox.Registry"

// WorkspacesDep is the dep name a sandbox resource uses to reach the
// workspace registry its sandboxes reference by name. Sandboxes resolve
// host roots through it, so this dependency is what forces a sandbox
// resource to be built after its workspaces — see the topological
// ordering in a deployment engine.
const WorkspacesDep = "workspaces"

// NewDeployFactory returns the deployment factory for sandbox
// registries.
//
// The workspace registry arrives through deps rather than settings
// because it is a live object shared with other consumers, not
// declarative data:
//
//	resources:
//	  boxes:
//	    kind: sandbox.Registry
//	    impl: yaml
//	    deps: {workspaces: files}
//	    settings: {file: ./sandboxes.yaml}
//
// The approval callback stays application-owned: the document cannot
// express a function, so a document needing gated commands must be
// built by a host that registers its own impl wrapping [NewBuilder]
// with an Approver in Deps.
func NewDeployFactory() config.ResourceFactory {
	return config.NewDocumentFactory(
		config.ResourceSpec{
			Kind: ResourceKind,
			Impl: "yaml",
			Deps: []config.ResourceDepSpec{{
				Name:     WorkspacesDep,
				Type:     workspaceconfig.ResourceKind,
				Required: true,
			}},
			ItemType: "sandbox.Runner",
		},
		func(ctx context.Context, data []byte, deps map[string]any) (any, error) {
			value, ok := deps[WorkspacesDep]
			if !ok {
				return nil, errdefs.Validationf(
					"sandbox config: dep %q is required", WorkspacesDep)
			}
			workspaces, ok := value.(*workspaceconfig.Registry)
			if !ok {
				return nil, errdefs.Validationf(
					"sandbox config: dep %q is %T, want *workspace/config.Registry",
					WorkspacesDep, value)
			}
			doc, err := Parse(data)
			if err != nil {
				return nil, err
			}
			return NewBuilder(Deps{Workspaces: workspaces}).Build(ctx, doc)
		},
	)
}
