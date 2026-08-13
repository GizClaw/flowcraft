package workspace

import (
	"context"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Settings is the strict settings subtree of the local workspace
// factory.
type Settings struct {
	Root   string          `json:"root"`
	Scoped *ScopedSettings `json:"scoped,omitempty"`
}

// ScopedSettings optionally wraps the local workspace in a
// [ScopedWorkspace]. The switch is explicit: the scope is applied only
// when Enabled is true.
type ScopedSettings struct {
	Enabled       bool     `json:"enabled"`
	DenyRead      []string `json:"deny_read,omitempty"`
	AllowWrite    []string `json:"allow_write,omitempty"`
	MandatoryDeny []string `json:"mandatory_deny,omitempty"`
}

// Factory builds a local directory workspace as the
// workspace.Workspace resource.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: "workspace.Workspace", Impl: "local"}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, err
	}
	if settings.Root == "" {
		return nil, errdefs.Validationf(
			"workspace: settings.root is required")
	}
	if !filepath.IsAbs(settings.Root) && in.Loader != nil {
		if base := in.Loader.BaseDir(); base != "" {
			settings.Root = filepath.Join(base, settings.Root)
		}
	}
	local, err := NewLocalWorkspace(settings.Root)
	if err != nil {
		return nil, errdefs.Validationf("workspace: %v", err)
	}
	var value Workspace = local
	if settings.Scoped != nil && settings.Scoped.Enabled {
		var opts []ScopedOption
		if len(settings.Scoped.DenyRead) > 0 {
			opts = append(opts, WithDenyRead(settings.Scoped.DenyRead...))
		}
		if len(settings.Scoped.AllowWrite) > 0 {
			opts = append(opts, WithAllowWrite(settings.Scoped.AllowWrite...))
		}
		if len(settings.Scoped.MandatoryDeny) > 0 {
			opts = append(opts, WithMandatoryDeny(settings.Scoped.MandatoryDeny...))
		}
		value = NewScopedWorkspace(value, opts...)
	}
	return value, nil
}

// Register adds the local workspace factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
