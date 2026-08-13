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
	opts := []resource.ExpandOption{
		resource.ExpandEnv(),
		resource.ExpandHome(),
	}
	base := in.Loader.BaseDir()
	if base != "" {
		abs, err := filepath.Abs(base)
		if err != nil {
			return nil, errdefs.Validationf(
				"workspace: resolve base dir: %v", err)
		}
		opts = append(opts, resource.ExpandBase(abs))
	}
	settings, err := resource.DecodeTyped[Settings](in.Settings, opts...)
	if err != nil {
		return nil, err
	}
	if settings.Root == "" {
		return nil, errdefs.Validationf(
			"workspace: settings.root is required")
	}
	if !filepath.IsAbs(settings.Root) && base != "" {
		settings.Root = filepath.Join(base, settings.Root)
	}
	local, err := NewLocalWorkspace(settings.Root)
	if err != nil {
		return nil, errdefs.Validationf("workspace: %v", err)
	}
	var value Workspace = local
	if settings.Scoped != nil && settings.Scoped.Enabled {
		var scopedOpts []ScopedOption
		if len(settings.Scoped.DenyRead) > 0 {
			scopedOpts = append(scopedOpts, WithDenyRead(settings.Scoped.DenyRead...))
		}
		if len(settings.Scoped.AllowWrite) > 0 {
			scopedOpts = append(scopedOpts, WithAllowWrite(settings.Scoped.AllowWrite...))
		}
		if len(settings.Scoped.MandatoryDeny) > 0 {
			scopedOpts = append(scopedOpts, WithMandatoryDeny(settings.Scoped.MandatoryDeny...))
		}
		value = NewScopedWorkspace(value, scopedOpts...)
	}
	return value, nil
}

// Register adds the local workspace factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
