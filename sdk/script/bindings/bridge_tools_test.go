package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func TestToolBridge_Allowed(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "echo", Description: "echo"},
		func(_ context.Context, args string) (string, error) {
			return "got:" + args, nil
		},
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg, bindings.WithAllowedToolNames("echo")),
	)
	_, err := rt.Exec(context.Background(), "tools", `
		var r = tools.call("echo", "{\"x\":1}");
		if (r.is_error) throw new Error(r.content);
		if (r.content.indexOf("got:") !== 0) throw new Error(r.content);
		var names = tools.list();
		if (names.length !== 1 || names[0] !== "echo") throw new Error("list");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_DenyByDefault(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "echo", Description: "echo"},
		func(_ context.Context, _ string) (string, error) { return "ok", nil },
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg),
	)
	_, err := rt.Exec(context.Background(), "deny", `
		var r = tools.call("echo", "{}");
		if (!r.is_error) throw new Error("expected deny");
		// list() with no allowlist must return an empty list (not all names).
		if (tools.list().length !== 0) throw new Error("list should be empty under default deny");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_AllowAll(t *testing.T) {
	// WithToolAllowAll bypasses the per-name allowlist — confirms the
	// "trusted scripts" escape hatch lets every registered tool through.
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "a", Description: "a"},
		func(_ context.Context, _ string) (string, error) { return "A", nil },
	))
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "b", Description: "b"},
		func(_ context.Context, _ string) (string, error) { return "B", nil },
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg, bindings.WithToolAllowAll()),
	)
	_, err := rt.Exec(context.Background(), "allowall", `
		var ra = tools.call("a", "{}");
		if (ra.is_error || ra.content !== "A") throw new Error("a failed: " + JSON.stringify(ra));
		var rb = tools.call("b", "{}");
		if (rb.is_error || rb.content !== "B") throw new Error("b failed: " + JSON.stringify(rb));

		// list() under AllowAll surfaces every registered tool name.
		var names = tools.list().slice().sort();
		if (names.length !== 2 || names[0] !== "a" || names[1] !== "b") {
			throw new Error("list mismatch: " + names);
		}
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_AllowAll_UnknownTool(t *testing.T) {
	// Even under AllowAll, calling a tool the registry doesn't know about
	// must surface as is_error (vs. silently invoking nil) — exercises the
	// "tools: unknown tool %q" branch of NewToolBridge.
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "known", Description: "k"},
		func(_ context.Context, _ string) (string, error) { return "ok", nil },
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg, bindings.WithToolAllowAll()),
	)
	_, err := rt.Exec(context.Background(), "allowall-unknown", `
		var r = tools.call("ghost", "{}");
		if (!r.is_error) throw new Error("expected is_error for unknown tool");
		if (String(r.content).indexOf("ghost") === -1) {
			throw new Error("error content should mention the missing tool name: " + r.content);
		}
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_NilRegistry(t *testing.T) {
	// nil registry must not panic; call() returns is_error and list() returns
	// an empty list. Mirrors the FS bridge's nil-workspace contract.
	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(nil, bindings.WithToolAllowAll()),
	)
	_, err := rt.Exec(context.Background(), "tools-nilreg", `
		var r = tools.call("anything", "{}");
		if (!r.is_error) throw new Error("expected is_error with nil registry");
		if (tools.list() !== null && tools.list().length !== 0) {
			throw new Error("list should be empty with nil registry");
		}
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}
