package bootstrap

import (
	"context"
	"path/filepath"
	"time"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// wireSandbox creates the workspace, sandbox manager, and registers sandbox
// and kanban tools. The returned cleanup closes the sandbox manager.
func wireSandbox(ctx context.Context, cfg *config.Config, toolReg *tool.Registry) (workspace.Workspace, *sandbox.Manager, sandbox.ManagerConfig, func(), error) {
	workspaceRoot := cfg.Sandbox.RootDir
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(cfg.ConfigurePath, "workspace")
	}

	ws, err := workspace.NewLocalWorkspace(workspaceRoot)
	if err != nil {
		telemetry.Error(ctx, "failed to initialize workspace", otellog.String("error", err.Error()))
		return nil, nil, sandbox.ManagerConfig{}, nil, err
	}

	sandboxCfg := sandbox.ManagerConfig{
		Driver:        cfg.Sandbox.Driver,
		Mode:          sandbox.ParseMode(cfg.Sandbox.Mode),
		RootDir:       workspaceRoot,
		Image:         cfg.Sandbox.Image,
		MaxConcurrent: cfg.Sandbox.MaxConcurrent,
		NetworkMode:   cfg.Sandbox.NetworkMode,
		CPUQuota:      cfg.Sandbox.CPUQuota,
		MemoryLimit:   cfg.Sandbox.MemoryLimit,
	}
	if sandboxCfg.Driver == "" {
		sandboxCfg.Driver = "local"
	}
	if sandboxCfg.MaxConcurrent <= 0 {
		sandboxCfg.MaxConcurrent = 10
	}
	if cfg.Sandbox.ExecTimeout != "" {
		if d, err := time.ParseDuration(cfg.Sandbox.ExecTimeout); err == nil {
			sandboxCfg.ExecTimeout = d
		}
	}
	if cfg.Sandbox.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.Sandbox.IdleTimeout); err == nil {
			sandboxCfg.IdleTimeout = d
		}
	}
	if sandboxCfg.Driver == "docker" {
		sandboxCfg.Mounts = buildSandboxMounts(cfg, workspaceRoot)
	}

	sm, err := sandbox.NewManager(ctx, sandboxCfg)
	if err != nil {
		telemetry.Error(ctx, "failed to initialize sandbox manager", otellog.String("error", err.Error()))
		return nil, nil, sandbox.ManagerConfig{}, nil, err
	}

	toolReg.Register(&sandbox.ExecTool{Manager: sm})
	toolReg.Register(&sandbox.ReadTool{Manager: sm})
	toolReg.Register(&sandbox.WriteTool{Manager: sm})
	toolReg.Register(&kanban.SubmitTool{})
	toolReg.Register(&kanban.TaskContextTool{})

	cleanup := func() { _ = sm.Close() }
	return ws, sm, sandboxCfg, cleanup, nil
}
