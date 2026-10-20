package workspace

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// ResourceKind is the deployment resource kind implemented by
// checkpoint stores. It is shared by every backend (workspace now,
// sqlite later) so runtime checkpoint wiring accepts either
// implementation.
const ResourceKind = "checkpoint.Store"

// Settings is the strict settings subtree of the workspace-backed
// checkpoint store factory.
type Settings struct {
	// Prefix is the workspace directory holding checkpoint files.
	// Empty uses the store default ("agent/checkpoints").
	Prefix string `json:"prefix,omitempty"`
}

// Factory builds a workspace-backed checkpoint store as the
// checkpoint.Store resource.
type Factory struct{}

// depWorkspace is the workspace this store persists under.
var depWorkspace = resource.Require[workspace.Workspace]("workspace")

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: ResourceKind,
		Impl: "workspace",
		Deps: []resource.Requirement{depWorkspace.Spec()},
	}
}

// New implements resource.Factory.
func (Factory) New(ctx context.Context, in resource.Input) (*Store, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint: decode settings: %w", err))
	}
	ws, err := depWorkspace.Get(in)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	var options []Option
	if settings.Prefix != "" {
		options = append(options, WithPrefix(settings.Prefix))
	}
	return New(ws, options...)
}

// Register adds the workspace-backed checkpoint store factory to the
// registry.
func Register(r *resource.Registry) error {
	return resource.Register(r, Factory{})
}
