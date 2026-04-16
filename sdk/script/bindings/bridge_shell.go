package bindings

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ShellOption configures a shell bridge.
type ShellOption func(*shellConfig)

type shellConfig struct {
	allowList map[string]bool
}

// WithAllowedCommands restricts the shell bridge to only execute the
// specified commands. When set, any command not in the list is rejected.
func WithAllowedCommands(cmds ...string) ShellOption {
	return func(c *shellConfig) {
		c.allowList = make(map[string]bool, len(cmds))
		for _, cmd := range cmds {
			c.allowList[cmd] = true
		}
	}
}

// NewShellBridge exposes shell execution as global "shell".
func NewShellBridge(runner workspace.CommandRunner, opts ...ShellOption) BindingFunc {
	cfg := &shellConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	return func(ctx context.Context) (string, any) {
		return "shell", map[string]any{
			"exec": func(cmd string, argsRaw ...string) (map[string]any, error) {
				if runner == nil {
					return map[string]any{"exit_code": -1, "stdout": "", "stderr": "shell: no command runner configured"}, nil
				}
				parts := strings.Fields(cmd)
				if len(parts) == 0 {
					return map[string]any{"exit_code": -1, "stdout": "", "stderr": "shell: empty command"}, nil
				}
				cmdName := parts[0]
				baseName := filepath.Base(cmdName)
				if cfg.allowList != nil && !cfg.allowList[cmdName] && !cfg.allowList[baseName] {
					return map[string]any{"exit_code": -1, "stdout": "", "stderr": "shell: command not allowed: " + cmdName}, nil
				}
				args := parts[1:]
				args = append(args, argsRaw...)
				result, err := runner.Exec(ctx, cmdName, args, workspace.ExecOptions{})
				if err != nil {
					return map[string]any{"exit_code": -1, "stdout": "", "stderr": err.Error()}, nil
				}
				return map[string]any{
					"exit_code": result.ExitCode,
					"stdout":    result.Stdout,
					"stderr":    result.Stderr,
				}, nil
			},
		}
	}
}
