package resource_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestEngineMountsScriptBindings(t *testing.T) {
	var got []string
	rt := scriptRuntimeStub{exec: func(_ context.Context, _ string, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		for name := range env.Bindings {
			got = append(got, name)
		}
		return nil, nil
	}}
	provider := bindings.ProviderFunc(func(inv bindings.Invocation) ([]bindings.Binding, error) {
		if inv.NodeID != "n1" {
			t.Errorf("invocation node = %q, want n1", inv.NodeID)
		}
		return []bindings.Binding{{
			Name:  "tokens",
			Value: map[string]any{"estimate": func() int { return 42 }},
		}}, nil
	})

	engine := buildCustomEngine(t,
		customNodeDefinition(t, "script", map[string]any{"runtime": "js", "source": "x"}),
		map[string]any{"script_runtime": rt, "script_bindings": provider})

	if err := runCustomEngine(t, engine, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	sort.Strings(got)
	if len(got) != 1 || got[0] != "tokens" {
		t.Fatalf("script globals = %v, want only the engine-level binding", got)
	}
}

func TestEngineRejectsNonProviderScriptBindings(t *testing.T) {
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(customNodeDefinition(t, "script", map[string]any{"runtime": "js", "source": "x"})) + `}`),
		Deps:     map[string]any{"script_runtime": scriptRuntimeStub{}, "script_bindings": "not a provider"},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation for a non-provider script_bindings dep", err)
	}
}

func TestStandardScriptBindingsResource(t *testing.T) {
	value, err := (graphresource.ScriptBindingsFactory{}).New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(bindings.Provider)
	if !ok {
		t.Fatalf("New returned %T, want bindings.Provider", value)
	}

	env, err := bindings.Assemble(bindings.Invocation{
		Context: context.Background(),
		Board:   agent.NewBoard(),
		Host:    agent.NoopHost{},
		RunInfo: agent.RunInfo{Identity: agent.Identity{AgentID: "a", RunID: "run-1"}},
		Runtime: scriptRuntimeStub{},
	}, nil, provider)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for _, want := range []string{"board", "host", "node", "run", "runtime", "tools"} {
		if _, ok := env.Bindings[want]; !ok {
			t.Fatalf("standard bindings = %v, want %q", env.Bindings, want)
		}
	}
}

// newToolAssembly builds an assembly over echo/other for policy tests.
func newToolAssembly(t *testing.T) *tool.Assembly {
	t.Helper()
	echo := tooltest.FuncTool("echo", "echoes", func(_ context.Context, args string) (string, error) {
		return "got:" + args, nil
	})
	other := tooltest.FuncTool("other", "other", func(context.Context, string) (string, error) {
		return "other", nil
	})
	asm, err := tool.NewAssembly([]tool.Source{tooltest.Source(echo, other)})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	return asm
}

// assembleStandard builds the standard surface through the resource
// factory and assembles one environment from it.
func assembleStandard(t *testing.T, settings string, deps map[string]any) *agent.ScriptEnv {
	t.Helper()
	value, err := (graphresource.ScriptBindingsFactory{}).New(context.Background(), resource.Input{
		Settings: []byte(settings),
		Deps:     deps,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := bindings.Assemble(bindings.Invocation{
		Context: context.Background(),
		Board:   agent.NewBoard(),
		Host:    agent.NoopHost{},
		RunInfo: agent.RunInfo{Identity: agent.Identity{AgentID: "a", RunID: "run-1"}},
		Runtime: scriptRuntimeStub{},
	}, nil, value.(bindings.Provider))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return env
}

func TestStandardScriptBindingsToolPolicy(t *testing.T) {
	env := assembleStandard(t, `{"tools":{"allow":["echo"]}}`,
		map[string]any{"tools": newToolAssembly(t)})
	tools, ok := env.Bindings["tools"].(map[string]any)
	if !ok {
		t.Fatalf("tools global = %T, want a script object", env.Bindings["tools"])
	}

	allowed, err := tools["call"].(func(string, string) (map[string]any, error))("echo", `{}`)
	if err != nil {
		t.Fatalf("tools.call: %v", err)
	}
	if allowed["is_error"] == true {
		t.Fatalf("echo denied by policy: %v", allowed)
	}
	denied, err := tools["call"].(func(string, string) (map[string]any, error))("other", `{}`)
	if err != nil {
		t.Fatalf("tools.call: %v", err)
	}
	if denied["is_error"] != true {
		t.Fatalf("other must stay denied, got %v", denied)
	}
}

func TestStandardScriptBindingsPolicyValidation(t *testing.T) {
	cases := map[string]struct {
		settings string
		deps     map[string]any
	}{
		"unknown tool":           {`{"tools":{"allow":["missing"]}}`, map[string]any{"tools": newToolAssembly(t)}},
		"allow with allow_all":   {`{"tools":{"allow":["echo"],"allow_all":true}}`, map[string]any{"tools": newToolAssembly(t)}},
		"tools without dep":      {`{"tools":{"allow":["echo"]}}`, nil},
		"fs without workspace":   {`{"fs":{"max_read_bytes":1024}}`, nil},
		"fs cap not positive":    {`{"fs":{"max_write_bytes":0}}`, map[string]any{"workspace": newWorkspace(t)}},
		"shell without sandbox":  {`{"shell":{"allow":["ls"]}}`, nil},
		"empty policy list item": {`{"tools":{"allow":[""]}}`, map[string]any{"tools": newToolAssembly(t)}},
	}
	for name, tc := range cases {
		_, err := (graphresource.ScriptBindingsFactory{}).New(context.Background(), resource.Input{
			Settings: []byte(tc.settings),
			Deps:     tc.deps,
		})
		if !errdefs.IsValidation(err) {
			t.Fatalf("%s: New = %v, want Validation", name, err)
		}
	}
}

func newWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func TestStandardScriptBindingsFSCap(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	env := assembleStandard(t, `{"fs":{"max_read_bytes":4}}`, map[string]any{"workspace": ws})
	fs, ok := env.Bindings["fs"].(map[string]any)
	if !ok {
		t.Fatalf("fs global = %T, want a script object", env.Bindings["fs"])
	}
	if _, err := fs["read"].(func(string) (string, error))("big.txt"); err == nil {
		t.Fatal("fs.read over the configured cap must fail")
	}
}

func TestScriptBindingsNoneBindsNothing(t *testing.T) {
	value, err := (graphresource.ScriptBindingsNoneFactory{}).New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := bindings.Assemble(bindings.Invocation{
		Context: context.Background(),
		Board:   agent.NewBoard(),
		Host:    agent.NoopHost{},
		Runtime: scriptRuntimeStub{},
	}, nil, value.(bindings.Provider))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(env.Bindings) != 0 {
		t.Fatalf("bindings = %v, want none", env.Bindings)
	}
}
