package config

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	yamlv3 "gopkg.in/yaml.v3"
)

// Built-in workspace driver names.
const (
	DriverLocal  = "local"
	DriverMemory = "memory"
)

func (b *Builder) registerBuiltins() {
	b.factories[DriverLocal] = b.buildLocal
	b.factories[DriverMemory] = buildMemory
}

type localSettings struct {
	Root string `yaml:"root"`
}

func (b *Builder) buildLocal(_ context.Context, settings *yamlv3.Node) (Resource, error) {
	value, err := DecodeSettings[localSettings](settings)
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

func buildMemory(_ context.Context, settings *yamlv3.Node) (Resource, error) {
	if _, err := DecodeSettings[memorySettings](settings); err != nil {
		return Resource{}, errdefs.Validationf(
			"decode %s settings: %v", DriverMemory, err)
	}
	return Resource{Workspace: workspace.NewMemWorkspace()}, nil
}
