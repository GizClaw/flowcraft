package pluginhost

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/plugin"
	pb "github.com/GizClaw/flowcraft/plugin/proto"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

type dummyTool struct {
	name string
}

func (d *dummyTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{Name: d.name, Description: "dummy"}
}

func (d *dummyTool) Execute(_ context.Context, _ string) (string, error) {
	return "dummy", nil
}

type fakePbToolService struct {
	pb.ToolServiceClient
}

type fakeToolPlugin struct {
	info  plugin.PluginInfo
	tools []plugin.ToolSpec
}

func (f *fakeToolPlugin) Info() plugin.PluginInfo                              { return f.info }
func (f *fakeToolPlugin) Initialize(_ context.Context, _ map[string]any) error { return nil }
func (f *fakeToolPlugin) Shutdown(_ context.Context) error                     { return nil }
func (f *fakeToolPlugin) Tools() []plugin.ToolSpec                             { return f.tools }
func (f *fakeToolPlugin) ExecuteTool(_ context.Context, _ string, _ string) (string, error) {
	return "ok", nil
}

func TestInjectTools_NoPlugins(t *testing.T) {
	reg := NewRegistry()
	toolReg := tool.NewRegistry()
	InjectTools(reg, toolReg)
	if toolReg.Len() != 0 {
		t.Fatalf("expected 0 tools, got %d", toolReg.Len())
	}
}

func TestInjectTools_SkipsBuiltinCollision(t *testing.T) {
	reg := NewRegistry()

	toolReg := tool.NewRegistry()
	toolReg.Register(&dummyTool{name: "echo"})

	ep := &ExternalPlugin{
		info:  plugin.PluginInfo{ID: "ext1"},
		tools: []plugin.ToolSpec{{Name: "echo", Description: "external echo"}},
	}
	reg.plugins["ext1"] = &managedPlugin{p: ep, status: plugin.StatusActive}

	InjectTools(reg, toolReg)

	if toolReg.Len() != 1 {
		t.Fatalf("expected 1 tool (builtin only), got %d", toolReg.Len())
	}
}

func TestInjectTools_InjectsExternalTool(t *testing.T) {
	reg := NewRegistry()
	toolReg := tool.NewRegistry()

	ep := &ExternalPlugin{
		info:    plugin.PluginInfo{ID: "ext1"},
		tools:   []plugin.ToolSpec{{Name: "ext_tool", Description: "an external tool"}},
		toolSvc: &fakePbToolService{},
	}
	reg.plugins["ext1"] = &managedPlugin{p: ep, status: plugin.StatusActive}

	InjectTools(reg, toolReg)

	if _, ok := toolReg.Get("ext_tool"); !ok {
		t.Fatal("expected ext_tool to be injected")
	}
	if toolReg.Len() != 1 {
		t.Fatalf("expected 1 tool, got %d", toolReg.Len())
	}
}

func TestInjectTools_SkipsInactivePlugin(t *testing.T) {
	reg := NewRegistry()
	toolReg := tool.NewRegistry()

	ep := &ExternalPlugin{
		info:    plugin.PluginInfo{ID: "ext1"},
		tools:   []plugin.ToolSpec{{Name: "ext_tool", Description: "test"}},
		toolSvc: &fakePbToolService{},
	}
	reg.plugins["ext1"] = &managedPlugin{p: ep, status: plugin.StatusInactive}

	InjectTools(reg, toolReg)

	if toolReg.Len() != 0 {
		t.Fatalf("expected 0 tools for inactive plugin, got %d", toolReg.Len())
	}
}

func TestCleanupSchemas_RemovesOrphaned(t *testing.T) {
	reg := NewRegistry()
	schemaReg := node.NewSchemaRegistry()

	schemaReg.Register(node.NodeSchema{Type: "orphan_node", Label: "Orphan", Category: "plugin"})
	schemaReg.Register(node.NodeSchema{Type: "llm", Label: "LLM", Category: "ai"})

	if schemaReg.Len() != 2 {
		t.Fatalf("expected 2, got %d", schemaReg.Len())
	}

	CleanupSchemas(reg, schemaReg)

	if schemaReg.Len() != 1 {
		t.Fatalf("expected 1 after cleanup, got %d", schemaReg.Len())
	}
	if _, ok := schemaReg.Get("orphan_node"); ok {
		t.Fatal("orphan_node should be removed")
	}
	if _, ok := schemaReg.Get("llm"); !ok {
		t.Fatal("builtin schema should remain")
	}
}

func TestCleanupSchemas_KeepsActivePluginSchemas(t *testing.T) {
	reg := NewRegistry()

	ep := &ExternalPlugin{
		info:  plugin.PluginInfo{ID: "ext1"},
		nodes: []plugin.NodeSpec{{Type: "active_node"}},
	}
	reg.plugins["ext1"] = &managedPlugin{p: ep, status: plugin.StatusActive}

	schemaReg := node.NewSchemaRegistry()
	schemaReg.Register(node.NodeSchema{Type: "active_node", Label: "Active", Category: "plugin"})
	schemaReg.Register(node.NodeSchema{Type: "orphan_node", Label: "Orphan", Category: "plugin"})

	CleanupSchemas(reg, schemaReg)

	if _, ok := schemaReg.Get("active_node"); !ok {
		t.Fatal("active plugin schema should remain")
	}
	if _, ok := schemaReg.Get("orphan_node"); ok {
		t.Fatal("orphan schema should be removed")
	}
}

func TestCleanupTools_RemovesOrphanedProxyTool(t *testing.T) {
	reg := NewRegistry()
	toolReg := tool.NewRegistry()

	proxy := NewProxyTool(plugin.ToolSpec{Name: "orphan_tool"}, "dead-plugin", nil)
	toolReg.Register(proxy)
	toolReg.Register(&dummyTool{name: "builtin_tool"})

	if toolReg.Len() != 2 {
		t.Fatalf("expected 2, got %d", toolReg.Len())
	}

	CleanupTools(reg, toolReg)

	if toolReg.Len() != 1 {
		t.Fatalf("expected 1 after cleanup, got %d", toolReg.Len())
	}
	if _, ok := toolReg.Get("orphan_tool"); ok {
		t.Fatal("orphan ProxyTool should be removed")
	}
	if _, ok := toolReg.Get("builtin_tool"); !ok {
		t.Fatal("builtin tool should remain")
	}
}

func TestCleanupTools_KeepsActivePluginTools(t *testing.T) {
	reg := NewRegistry()

	fp := &fakeToolPlugin{
		info:  plugin.PluginInfo{ID: "ext1"},
		tools: []plugin.ToolSpec{{Name: "active_tool"}},
	}
	reg.plugins["ext1"] = &managedPlugin{p: fp, status: plugin.StatusActive}

	toolReg := tool.NewRegistry()
	proxyActive := NewProxyTool(plugin.ToolSpec{Name: "active_tool"}, "ext1", nil)
	toolReg.Register(proxyActive)
	proxyOrphan := NewProxyTool(plugin.ToolSpec{Name: "orphan_tool"}, "dead-plugin", nil)
	toolReg.Register(proxyOrphan)

	CleanupTools(reg, toolReg)

	if _, ok := toolReg.Get("active_tool"); !ok {
		t.Fatal("active plugin tool should remain")
	}
	if _, ok := toolReg.Get("orphan_tool"); ok {
		t.Fatal("orphan tool should be removed")
	}
}
