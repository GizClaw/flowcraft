package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// ExecTool executes commands in the sandbox.
type ExecTool struct {
	Manager *Manager
}

func (*ExecTool) SelfTimeout() bool { return true }

func (t *ExecTool) Definition() llm.ToolDefinition {
	return tool.DefineSchema("sandbox_bash",
		"Execute a shell command in the sandbox container. "+
			"Pass a complete shell command string, e.g. \"ls -la /tmp\" or \"curl -s https://example.com\". "+
			"Pipes, redirects, and chaining (&&, ||, ;) are supported.",
		tool.Property("command", "string", "The full shell command to execute, e.g. \"pip install requests && python main.py\""),
		tool.Property("work_dir", "string", "Working directory (relative to /workspace)"),
		tool.Property("timeout", "string", "Execution timeout as a Go duration string, e.g. \"30s\", \"5m\", \"1h\". Defaults to 5m if omitted. Choose based on expected command duration."),
	).Required("command").Build()
}

func (t *ExecTool) Execute(ctx context.Context, arguments string) (string, error) {
	sb, release, cfg, err := t.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	var args struct {
		Command string `json:"command"`
		WorkDir string `json:"work_dir"`
		Timeout string `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("sandbox_bash: parse args: %w", err)
	}

	timeout := cfg.ExecTimeout
	if args.Timeout != "" {
		if d, parseErr := time.ParseDuration(args.Timeout); parseErr == nil && d > 0 {
			timeout = d
		}
	}
	const maxTimeout = 30 * time.Minute
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	result, err := sb.Exec(ctx, "sh", []string{"-c", args.Command}, ExecOptions{
		WorkDir: args.WorkDir,
		Timeout: timeout,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox_bash: %w", err)
	}

	output, _ := json.Marshal(result)
	return string(output), nil
}

// ReadTool reads a file from the sandbox.
type ReadTool struct {
	Manager *Manager
}

func (t *ReadTool) Definition() llm.ToolDefinition {
	return tool.DefineSchema("sandbox_read", "Read a file from the project sandbox.",
		tool.Property("path", "string", "File path relative to sandbox root"),
	).Required("path").Build()
}

func (t *ReadTool) Execute(ctx context.Context, arguments string) (string, error) {
	sb, release, _, err := t.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("sandbox_read: parse args: %w", err)
	}

	data, err := sb.ReadFile(ctx, args.Path)
	if err != nil {
		return "", fmt.Errorf("sandbox_read: %w", err)
	}
	return string(data), nil
}

// WriteTool writes a file to the sandbox.
type WriteTool struct {
	Manager *Manager
}

func (t *WriteTool) Definition() llm.ToolDefinition {
	return tool.DefineSchema("sandbox_write", "Write or overwrite a file in the project sandbox. Parent directories are created automatically.",
		tool.Property("path", "string", "File path relative to sandbox root"),
		tool.Property("content", "string", "File content to write"),
	).Required("path", "content").Build()
}

func (t *WriteTool) Execute(ctx context.Context, arguments string) (string, error) {
	sb, release, _, err := t.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("sandbox_write: parse args: %w", err)
	}

	if err := sb.WriteFile(ctx, args.Path, []byte(args.Content)); err != nil {
		return "", fmt.Errorf("sandbox_write: %w", err)
	}
	return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
}

// acquire resolves the sandbox for the current user runtime.
// The Manager is injected via struct field; runtimeID comes from context.
func (t *ExecTool) acquire(ctx context.Context) (Sandbox, func(), ManagerConfig, error) {
	return acquireFromManager(ctx, t.Manager)
}

func (t *ReadTool) acquire(ctx context.Context) (Sandbox, func(), ManagerConfig, error) {
	return acquireFromManager(ctx, t.Manager)
}

func (t *WriteTool) acquire(ctx context.Context) (Sandbox, func(), ManagerConfig, error) {
	return acquireFromManager(ctx, t.Manager)
}

func acquireFromManager(ctx context.Context, mgr *Manager) (Sandbox, func(), ManagerConfig, error) {
	if handle, ok := model.SandboxHandleFrom(ctx).(*SandboxHandle); ok && handle != nil {
		sb, release, err := handle.Acquire(ctx)
		if err != nil {
			return nil, nil, ManagerConfig{}, fmt.Errorf("sandbox: acquire handle: %w", err)
		}
		return sb, release, handle.Config(), nil
	}

	if mgr == nil {
		return nil, nil, ManagerConfig{}, fmt.Errorf("sandbox: manager not available")
	}

	runtimeID := model.RuntimeIDFrom(ctx)
	if runtimeID == "" {
		return nil, nil, ManagerConfig{}, fmt.Errorf("sandbox: no runtime ID in context")
	}

	sb, err := mgr.Acquire(ctx, runtimeID, AcquireOptions{Mode: ModePersistent})
	if err != nil {
		return nil, nil, ManagerConfig{}, fmt.Errorf("sandbox: acquire: %w", err)
	}
	release := func() { _ = mgr.Release(runtimeID) }
	return sb, release, mgr.Config(), nil
}
