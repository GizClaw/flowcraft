package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/workspace/config"
)

// ResourceKind is the deploy resource category this package builds. A
// dep bound to the whole resource must declare this as its Type; a dep
// spelled "name/item" binds one sandbox.Runner out of it, because
// [Registry] is a container (see [Registry.Lookup]).
const ResourceKind = "sandbox.Registry"

// WorkspacesDep is the dep name a sandbox resource uses to reach the
// workspace registry its sandboxes reference by name. Sandboxes resolve
// host roots through it, so this dependency is what forces a sandbox
// resource to be built after its workspaces — see the topological
// ordering in sdkx/deploy.
const WorkspacesDep = "workspaces"

// ResourceSettings is the settings subtree of a sandbox resource.
type ResourceSettings struct {
	deploy.SubDocument `yaml:",inline"`
}

// DeployResource builds a sandbox [Registry] as a sdkx/deploy resource,
// owned and closed by the deployment document.
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
// The approval callback stays application-owned: YAML cannot express a
// function, so a document needing gated commands must be built by a
// host that registers its own impl wrapping [NewBuilder] with an
// Approver in Deps.
func DeployResource(ctx context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"sandbox config: decode resource settings: %w", err))
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
	data, err := settings.YAML()
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return NewBuilder(Deps{Workspaces: workspaces}).Build(ctx, doc)
}
