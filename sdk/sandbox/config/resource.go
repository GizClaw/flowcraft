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

// Spec implements config.Factory. The workspace registry arrives
// through deps rather than settings because it is a live object shared
// with other consumers, not declarative data:
//
//	resources:
//	  boxes:
//	    kind: sandbox.Registry
//	    impl: yaml
//	    deps: {workspaces: files}
//	    settings: {file: ./sandboxes.yaml}
func (b *Builder) Spec() config.Spec {
	return config.Spec{
		Kind: ResourceKind,
		Impl: "yaml",
		Deps: []config.DepSpec{{
			Name:     WorkspacesDep,
			Type:     workspaceconfig.ResourceKind,
			Required: true,
		}},
		ItemType: "sandbox.Runner",
	}
}

// New implements config.Factory: it takes the workspace registry from
// the resolved deps, resolves the sandbox document from settings, and
// builds over the host builder's backend catalog.
func (b *Builder) New(ctx context.Context, in config.Input) (any, error) {
	if b == nil {
		return nil, errdefs.Validationf(
			"sandbox config: builder is nil")
	}
	value, ok := in.Dep(WorkspacesDep)
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
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	run := &Builder{
		deps:     Deps{Workspaces: workspaces, Approver: b.deps.Approver},
		backends: b.backends,
	}
	return run.Build(ctx, doc)
}
