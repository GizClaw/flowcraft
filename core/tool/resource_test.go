package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestRegistryFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := tool.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup(tool.RegistryKind, "memory")
	if !ok {
		t.Fatal("tool.Registry/memory factory not registered")
	}
	spec := factory.Spec()
	if len(spec.Deps) != 1 || spec.Deps[0].Name != "tool" ||
		spec.Deps[0].Type != "tool.Source" || !spec.Deps[0].Many {
		t.Fatalf("spec deps = %+v, want Many tool.Source dep", spec.Deps)
	}

	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			"tool":    source{tools: []tool.Tool{funcTool("a", "1")}},
			"tool.bb": source{tools: []tool.Tool{funcTool("b", "2")}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registry, ok := value.(*tool.Registry)
	if !ok {
		t.Fatalf("New returned %T, want *tool.Registry", value)
	}
	if registry.Len() != 2 {
		t.Fatalf("Len = %d, want 2", registry.Len())
	}
}

func TestRegistryFactoryRequiresSources(t *testing.T) {
	_, err := (tool.RegistryFactory{}).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without sources = %v, want Validation", err)
	}
}

func TestRegistryFactoryRejectsWrongDepType(t *testing.T) {
	_, err := (tool.RegistryFactory{}).New(context.Background(), resource.Input{
		Deps: map[string]any{"tool": "not a source"},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong dep = %v, want Validation", err)
	}
}

func TestAssemblyFactoryWithDynamic(t *testing.T) {
	reg := resource.NewRegistry()
	if err := tool.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup(tool.AssemblyKind, "memory")
	if !ok {
		t.Fatal("tool.Assembly/memory factory not registered")
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{
			"dynamic": {"default": "deferred", "exposures": {"search": "always"}}
		}`),
		Deps: map[string]any{
			"tool": source{tools: []tool.Tool{funcTool("search", "x")}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly, ok := value.(*tool.Assembly)
	if !ok {
		t.Fatalf("New returned %T, want *tool.Assembly", value)
	}
	session := assembly.NewSession()
	names := definitionNames(session.Definitions())
	if !contains(names, tool.ToolName) || !contains(names, "search") {
		t.Fatalf("definitions = %v, want tool_search + search", names)
	}
}

func TestAssemblyFactoryRejectsMiddlewaresSettings(t *testing.T) {
	_, err := (tool.AssemblyFactory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"middlewares": {"recover": {"enabled": true}}}`),
		Deps: map[string]any{
			"tool": source{tools: []tool.Tool{funcTool("a", "1")}},
		},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("memory assembly with middlewares = %v, want Validation", err)
	}
}

func TestAssemblyFactoryRequiresSources(t *testing.T) {
	_, err := (tool.AssemblyFactory{}).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without sources = %v, want Validation", err)
	}
}
