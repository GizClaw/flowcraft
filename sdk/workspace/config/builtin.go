package config

import (
	"context"
	"fmt"
	"path/filepath"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Built-in workspace driver names.
const (
	DriverLocal  = "local"
	DriverMemory = "memory"
)

func (b *Builder) registerBuiltins() {
	if err := b.catalog.Register(DriverLocal, b.buildLocal); err != nil {
		panic(err)
	}
	if err := b.catalog.Register(DriverMemory, buildMemory); err != nil {
		panic(err)
	}
}

type localSettings struct {
	Root string `json:"root"`
}

func (b *Builder) buildLocal(_ context.Context, in sdkconfig.Input) (Resource, error) {
	value, err := sdkconfig.DecodeSettings[localSettings](in.Settings)
	if err != nil {
		return Resource{}, errdefs.Validationf(
			"decode %s settings: %v", DriverLocal, err)
	}
	if value.Root == "" {
		return Resource{}, errdefs.Validationf(
			"%s settings.root is required", DriverLocal)
	}
	root := value.Root
	if !filepath.IsAbs(root) {
		root = filepath.Join(b.deps.BaseDir, root)
	}
	ws, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		return Resource{}, fmt.Errorf("open local root %q: %w", root, err)
	}
	return Resource{Workspace: ws, Root: ws.Root()}, nil
}

type memorySettings struct{}

func buildMemory(_ context.Context, in sdkconfig.Input) (Resource, error) {
	_, err := sdkconfig.DecodeSettings[memorySettings](in.Settings)
	if err != nil {
		return Resource{}, errdefs.Validationf(
			"decode %s settings: %v", DriverMemory, err)
	}
	return Resource{Workspace: workspace.NewMemWorkspace()}, nil
}
