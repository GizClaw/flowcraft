package scriptnode

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	nodepkg "github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

type contextCommandRunner struct {
	stdout   string
	stderr   string
	exitCode int
}

func (r contextCommandRunner) Exec(context.Context, string, []string, sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{
		Stdout:   r.stdout,
		Stderr:   r.stderr,
		ExitCode: r.exitCode,
	}, nil
}

func TestBuiltin_Context_CommandFailureSignalsTypedError(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	runner := contextCommandRunner{stdout: "ignored", stderr: "command failed", exitCode: 2}
	n := New("ctx1", "context", scripts.MustGet("context"), map[string]any{
		"commands": []any{
			map[string]any{"command": "false", "state_key": "cmd_out"},
		},
	}, rt, bindings.NewShellBridge(runner))

	board := graph.NewBoard()
	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		RunID:   runIDForTests,
	}, board)
	if !errdefs.IsInternal(err) {
		t.Fatalf("ExecuteBoard error = %v, want Internal", err)
	}
	if v, ok := board.GetVar("cmd_out"); ok && v != nil {
		t.Fatalf("failed command stdout should not be written, got %v", v)
	}
}

func TestBuiltin_Context_CommandUnavailableSignalsNotAvailable(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	runner := contextCommandRunner{stderr: "runner offline", exitCode: -1}
	n := New("ctx_unavailable", "context", scripts.MustGet("context"), map[string]any{
		"commands": []any{
			map[string]any{"command": "tool status", "state_key": "cmd_out"},
		},
	}, rt, bindings.NewShellBridge(runner))

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		RunID:   runIDForTests,
	}, graph.NewBoard())
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("ExecuteBoard error = %v, want NotAvailable", err)
	}
}

func TestRegister_ContextFilesOnlyBuildsWithoutCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	_, err := factory.Build(graph.NodeDefinition{
		ID:   "ctx_files",
		Type: "context",
		Config: map[string]any{
			"files": []any{
				map[string]any{"path": "README.md"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build files-only context without CommandRunner: %v", err)
	}
}

func TestRegister_GateWithoutCommandsBuildsWithoutCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	_, err := factory.Build(graph.NodeDefinition{
		ID:     "gate_empty",
		Type:   "gate",
		Config: map[string]any{"commands": []any{}},
	})
	if err != nil {
		t.Fatalf("Build commandless gate without CommandRunner: %v", err)
	}
}

func TestRegister_BuiltinCommandsRequireCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	for _, nodeType := range []string{"context", "gate"} {
		t.Run(nodeType, func(t *testing.T) {
			_, err := factory.Build(graph.NodeDefinition{
				ID:     nodeType + "1",
				Type:   nodeType,
				Config: map[string]any{"commands": []any{"echo hi"}},
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want Validation", err)
			}
		})
	}
}
