package config

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type Builder struct {
	workspace workspace.Workspace
	inference *inference.Runtime
}

// NewBuilder binds borrowed infrastructure. It starts no goroutines.
func NewBuilder(ws workspace.Workspace, runtime *inference.Runtime) (*Builder, error) {
	if nilInterface(ws) {
		return nil, errors.New("memory config: workspace is required")
	}
	if runtime == nil {
		return nil, errors.New("memory config: inference runtime is required")
	}
	return &Builder{workspace: ws, inference: runtime}, nil
}
