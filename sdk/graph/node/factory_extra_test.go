package node

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// --- stub types for BuildContext options ---

type stubResolver struct{}

func (s *stubResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) { return nil, nil }
func (s *stubResolver) InvalidateCache(_ string)                             {}

type stubRuntime struct{}

func (s *stubRuntime) Exec(_ context.Context, _, _ string, _ *script.Env) (*script.Signal, error) {
	return nil, nil
}

type stubWorkspace struct{ workspace.Workspace }
type stubCommandRunner struct{ workspace.CommandRunner }

// --- FactoryOption coverage ---

func TestWithLLMResolver(t *testing.T) {
	r := &stubResolver{}
	f := NewFactory(WithLLMResolver(r))
	if f.buildCtx.LLMResolver == nil {
		t.Fatal("LLMResolver not set")
	}
}

func TestWithToolRegistry(t *testing.T) {
	tr := tool.NewRegistry()
	f := NewFactory(WithToolRegistry(tr))
	if f.buildCtx.ToolRegistry == nil {
		t.Fatal("ToolRegistry not set")
	}
}

func TestWithScriptRuntime(t *testing.T) {
	rt := &stubRuntime{}
	f := NewFactory(WithScriptRuntime(rt))
	if f.buildCtx.ScriptRuntime == nil {
		t.Fatal("ScriptRuntime not set")
	}
}

func TestWithScriptFS(t *testing.T) {
	fsys := fstest.MapFS{"test.txt": &fstest.MapFile{Data: []byte("hi")}}
	f := NewFactory(WithScriptFS(fsys))
	if f.buildCtx.ScriptFS == nil {
		t.Fatal("ScriptFS not set")
	}
	if _, ok := f.buildCtx.ScriptFS.(fs.FS); !ok {
		t.Fatal("ScriptFS should implement fs.FS")
	}
}

func TestWithWorkspace(t *testing.T) {
	ws := &stubWorkspace{}
	f := NewFactory(WithWorkspace(ws))
	if f.buildCtx.Workspace == nil {
		t.Fatal("Workspace not set")
	}
}

func TestWithCommandRunner(t *testing.T) {
	cr := &stubCommandRunner{}
	f := NewFactory(WithCommandRunner(cr))
	if f.buildCtx.CommandRunner == nil {
		t.Fatal("CommandRunner not set")
	}
}

func TestWithMultipleOptions(t *testing.T) {
	r := &stubResolver{}
	tr := tool.NewRegistry()
	rt := &stubRuntime{}
	f := NewFactory(WithLLMResolver(r), WithToolRegistry(tr), WithScriptRuntime(rt))
	if f.buildCtx.LLMResolver == nil || f.buildCtx.ToolRegistry == nil || f.buildCtx.ScriptRuntime == nil {
		t.Fatal("expected all three options to be applied")
	}
}

// --- RegisterFallbackBuilder (package-level) ---

func TestRegisterFallbackBuilder(t *testing.T) {
	old := defaultFallbackBuilder
	defer func() {
		defaultBuildersMu.Lock()
		defaultFallbackBuilder = old
		defaultBuildersMu.Unlock()
	}()

	fb := func(def graph.NodeDefinition, bctx *BuildContext) (graph.Node, error) {
		return graph.NewPassthroughNode("fb", "fallback"), nil
	}
	RegisterFallbackBuilder(fb)

	f := NewFactory()
	if f.Fallback() == nil {
		t.Fatal("NewFactory should copy default fallback builder")
	}
	n, err := f.Build(graph.NodeDefinition{ID: "x", Type: "unregistered"})
	if err != nil {
		t.Fatalf("expected fallback to handle unknown type: %v", err)
	}
	if n.ID() != "fb" {
		t.Fatalf("expected ID 'fb', got %q", n.ID())
	}
}

// --- buildLLMNode via Factory ---

func TestBuildLLMNode_NilResolver(t *testing.T) {
	f := emptyFactory()
	f.RegisterBuilder("llm", buildLLMNode)
	_, err := f.Build(graph.NodeDefinition{ID: "llm1", Type: "llm"})
	if err == nil {
		t.Fatal("expected error when LLMResolver is nil")
	}
}

func TestBuildLLMNode_Success(t *testing.T) {
	f := NewFactory(WithLLMResolver(&stubResolver{}))
	f.RegisterBuilder("llm", buildLLMNode)
	n, err := f.Build(graph.NodeDefinition{
		ID:   "llm1",
		Type: "llm",
		Config: map[string]any{
			"system_prompt": "be helpful",
			"temperature":   0.5,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.ID() != "llm1" {
		t.Fatalf("ID = %q, want llm1", n.ID())
	}
	if n.Type() != "llm" {
		t.Fatalf("Type = %q, want llm", n.Type())
	}
}

// --- SchemaRegistry.RegisterMany ---

func TestSchemaRegistry_RegisterMany(t *testing.T) {
	reg := NewSchemaRegistry()
	reg.RegisterMany([]NodeSchema{
		{Type: "a", Label: "A"},
		{Type: "b", Label: "B"},
		{Type: "c", Label: "C"},
	})
	if reg.Len() != 3 {
		t.Fatalf("expected 3, got %d", reg.Len())
	}
	all := reg.All()
	if all[0].Type != "a" || all[1].Type != "b" || all[2].Type != "c" {
		t.Fatalf("order mismatch: %v", all)
	}
}

// --- PortsForType for unknown type ---

func TestPortsForType_Unknown(t *testing.T) {
	input, output := PortsForType("__no_such_type__")
	if input != nil || output != nil {
		t.Fatalf("expected nil ports for unknown type, got input=%v output=%v", input, output)
	}
}

// --- convertPorts with empty input ---

func TestConvertPorts_Empty(t *testing.T) {
	result := convertPorts(nil)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
	result = convertPorts([]PortSchema{})
	if result != nil {
		t.Fatalf("expected nil for empty slice, got %v", result)
	}
}
