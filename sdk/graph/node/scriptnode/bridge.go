package scriptnode

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// BindingFunc creates a named binding for script execution.
// The returned name becomes the global variable name in the script scope,
// and the value is typically a map[string]any of callable Go functions.
type BindingFunc func(ctx context.Context) (name string, value any)

// BuildEnv creates a script.Env from binding funcs evaluated against ctx.
func BuildEnv(ctx context.Context, config map[string]any, fns ...BindingFunc) *script.Env {
	bindings := make(map[string]any, len(fns))
	for _, fn := range fns {
		name, val := fn(ctx)
		bindings[name] = val
	}
	return &script.Env{Config: config, Bindings: bindings}
}

// --- Board Bridge ---

func NewBoardBridge(board *graph.Board) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "board", map[string]any{
			"getVar":  func(key string) any { v, _ := board.GetVar(key); return v },
			"setVar":  func(key string, value any) { board.SetVar(key, value) },
			"getVars": func() map[string]any { return board.Vars() },
			"hasVar":  func(key string) bool { _, ok := board.GetVar(key); return ok },
		}
	}
}

// --- Stream Bridge ---

func NewStreamBridge(stream graph.StreamCallback, nodeID string) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "stream", map[string]any{
			"emit": func(eventType string, payload any) {
				if stream != nil {
					stream(graph.StreamEvent{Type: eventType, NodeID: nodeID, Payload: payload})
				}
			},
		}
	}
}

// --- Expr Bridge ---

func NewExprBridge() BindingFunc {
	return func(_ context.Context) (string, any) {
		return "expr", map[string]any{
			"eval": func(expression string, env map[string]any) (any, error) {
				return evalExpr(expression, env)
			},
		}
	}
}

// --- Shell Bridge ---

// ShellBridgeOption configures a shell bridge.
type ShellBridgeOption func(*shellBridgeConfig)

type shellBridgeConfig struct {
	allowList map[string]bool
}

// WithAllowedCommands restricts the shell bridge to only execute the
// specified commands. When set, any command not in the list is rejected.
func WithAllowedCommands(cmds ...string) ShellBridgeOption {
	return func(c *shellBridgeConfig) {
		c.allowList = make(map[string]bool, len(cmds))
		for _, cmd := range cmds {
			c.allowList[cmd] = true
		}
	}
}

// NewShellBridge creates a binding that exposes shell command execution to scripts.
func NewShellBridge(runner workspace.CommandRunner, opts ...ShellBridgeOption) BindingFunc {
	cfg := &shellBridgeConfig{}
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
				if cfg.allowList != nil && !cfg.allowList[cmdName] {
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

// --- FS Bridge ---

// NewFSBridge creates a binding that exposes file read/write operations to scripts.
func NewFSBridge(ws workspace.Workspace) BindingFunc {
	return func(ctx context.Context) (string, any) {
		return "fs", map[string]any{
			"read": func(path string) (string, error) {
				if ws == nil {
					return "", nil
				}
				data, err := ws.Read(ctx, path)
				if err != nil {
					return "", err
				}
				return string(data), nil
			},
			"write": func(path, content string) error {
				if ws == nil {
					return nil
				}
				return ws.Write(ctx, path, []byte(content))
			},
			"exists": func(path string) bool {
				if ws == nil {
					return false
				}
				ok, _ := ws.Exists(ctx, path)
				return ok
			},
			"delete": func(path string) error {
				if ws == nil {
					return nil
				}
				return ws.Delete(ctx, path)
			},
		}
	}
}

// --- Runtime Bridge ---

// runtimeBindings creates the "runtime" binding that allows scripts to execute
// sub-scripts. Parent bindings are automatically inherited by sub-scripts.
func runtimeBindings(ctx context.Context, rt script.Runtime, parentBindings map[string]any) map[string]any {
	return map[string]any{
		"execScript": func(source string, config map[string]any) (*script.Signal, error) {
			env := &script.Env{
				Config:   config,
				Bindings: parentBindings,
			}
			return rt.Exec(ctx, "inline", source, env)
		},
	}
}
