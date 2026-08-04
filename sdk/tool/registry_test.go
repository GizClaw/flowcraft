package tool

import (
	"context"
	"testing"
)

func stubTool(name string) Tool {
	return FuncTool(Definition{Name: name, Description: name + " desc"}, func(_ context.Context, _ string) (string, error) {
		return "{}", nil
	})
}

func TestRegister_DefaultScopeIsAgent(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	if got := r.ScopeOf("foo"); got != ScopeAgent {
		t.Errorf("ScopeOf(foo) = %q, want %q", got, ScopeAgent)
	}
}

func TestRegisterWithScope_Platform(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("bar"), ScopePlatform)

	if got := r.ScopeOf("bar"); got != ScopePlatform {
		t.Errorf("ScopeOf(bar) = %q, want %q", got, ScopePlatform)
	}
}

func TestScopeOf_UnknownTool_ReturnsAgent(t *testing.T) {
	r := NewRegistry()
	if got := r.ScopeOf("nonexistent"); got != ScopeAgent {
		t.Errorf("ScopeOf(nonexistent) = %q, want %q", got, ScopeAgent)
	}
}

func TestDefinitionsByScope(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("agent_tool_1"))
	r.Register(stubTool("agent_tool_2"))
	r.RegisterWithScope(stubTool("platform_tool_1"), ScopePlatform)
	r.RegisterWithScope(stubTool("platform_tool_2"), ScopePlatform)
	r.RegisterWithScope(stubTool("platform_tool_3"), ScopePlatform)

	agentDefs := r.DefinitionsByScope(ScopeAgent)
	if len(agentDefs) != 2 {
		t.Errorf("DefinitionsByScope(agent) returned %d items, want 2", len(agentDefs))
	}

	platformDefs := r.DefinitionsByScope(ScopePlatform)
	if len(platformDefs) != 3 {
		t.Errorf("DefinitionsByScope(platform) returned %d items, want 3", len(platformDefs))
	}

	allDefs := r.Definitions()
	if len(allDefs) != 5 {
		t.Errorf("Definitions() returned %d items, want 5", len(allDefs))
	}
}

func TestUnregister_RemovesScope(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("tmp"), ScopePlatform)

	if !r.Unregister("tmp") {
		t.Fatal("Unregister returned false for existing tool")
	}
	if _, ok := r.Get("tmp"); ok {
		t.Error("tool still found after Unregister")
	}
	if got := r.ScopeOf("tmp"); got != ScopeAgent {
		t.Errorf("ScopeOf(tmp) after Unregister = %q, want %q (default)", got, ScopeAgent)
	}
}

func TestUnregister_NonExistent(t *testing.T) {
	r := NewRegistry()
	if r.Unregister("nope") {
		t.Error("Unregister should return false for non-existent tool")
	}
}

func TestRegisterAllIfAbsentIsAtomicAndReleasePreservesReplacement(t *testing.T) {
	r := NewRegistry()
	existing := stubTool("existing")
	r.Register(existing)

	if release, ok := r.RegisterAllIfAbsent(stubTool("new"), stubTool("existing")); ok || release != nil {
		t.Fatal("batch with a conflicting name was registered")
	}
	if _, ok := r.Get("new"); ok {
		t.Fatal("non-conflicting member of rejected batch was registered")
	}
	if got, _ := r.Get("existing"); got != existing {
		t.Fatal("rejected batch replaced the existing tool")
	}

	owned := stubTool("owned")
	release, ok := r.RegisterAllIfAbsent(owned)
	if !ok || release == nil {
		t.Fatal("conflict-free batch was rejected")
	}
	replacement := stubTool("owned")
	r.Register(replacement)
	release()
	release()
	if got, ok := r.Get("owned"); !ok || got != replacement {
		t.Fatal("release removed a later replacement")
	}
}

func TestDefinitionsByScope_Empty(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("only_platform"), ScopePlatform)

	agentDefs := r.DefinitionsByScope(ScopeAgent)
	if agentDefs != nil {
		t.Errorf("DefinitionsByScope(agent) = %v, want nil", agentDefs)
	}
}

func TestRegister_OverwritePreservesNewScope(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("x"))

	if got := r.ScopeOf("x"); got != ScopeAgent {
		t.Fatalf("initial scope = %q, want %q", got, ScopeAgent)
	}

	r.RegisterWithScope(stubTool("x"), ScopePlatform)
	if got := r.ScopeOf("x"); got != ScopePlatform {
		t.Errorf("after re-register scope = %q, want %q", got, ScopePlatform)
	}
}

func TestGet_ReturnsRegisteredTool(t *testing.T) {
	r := NewRegistry()
	want := stubTool("echo")
	r.Register(want)

	got, ok := r.Get("echo")
	if !ok {
		t.Fatal("Get(echo) not found")
	}
	if got != want {
		t.Error("Get(echo) returned a different tool instance")
	}
}

func TestNames(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("alpha"))
	r.Register(stubTool("beta"))

	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("len(Names) = %d, want 2", len(names))
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["alpha"] || !nameSet["beta"] {
		t.Errorf("Names = %v, want {alpha, beta}", names)
	}
}

func TestLen(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("empty Len = %d, want 0", r.Len())
	}
	r.Register(stubTool("a"))
	r.Register(stubTool("b"))
	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
}
