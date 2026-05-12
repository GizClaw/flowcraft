package llmnode

import (
	"reflect"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// TestNode_ResolveTools_RunDepsRegistryWinsOverConstructor closes
// the agent.Run plumbing path: when the upstream wires a
// *tool.Registry into engine.Run.Deps under depname.ToolRegistry,
// llmnode MUST use it in preference to the constructor-bound
// registry. Without this rule agent.Run could not swap the registry
// per-run (the historical builder-closure bug — contract-audit #2
// for tools).
func TestNode_ResolveTools_RunDepsRegistryWinsOverConstructor(t *testing.T) {
	ctorReg := tool.NewRegistry()
	ctorReg.Register(&mockTool{name: "ctor_only"})
	runReg := tool.NewRegistry()
	runReg.Register(&mockTool{name: "run_only"})

	deps := &engine.Dependencies{}
	deps.Set(depname.ToolRegistry, runReg)

	n := New("n1", nil, ctorReg, Config{ToolNames: []string{"run_only"}})
	gotReg, gotAllow := n.resolveTools(graph.ExecutionContext{Deps: deps})

	if gotReg != runReg {
		t.Fatalf("registry: got %p want %p (run-scoped should win)", gotReg, runReg)
	}
	if !reflect.DeepEqual(gotAllow, []string{"run_only"}) {
		t.Errorf("allow list: got %v want [run_only]", gotAllow)
	}
}

// TestNode_ResolveTools_FallsBackToConstructorWhenNoDeps documents
// the back-compat path: callers that build llmnode without any
// agent.Run wiring (vessel inline engine, hand-built test graphs)
// keep the legacy "registry comes from the constructor" behaviour.
func TestNode_ResolveTools_FallsBackToConstructorWhenNoDeps(t *testing.T) {
	ctorReg := tool.NewRegistry()
	ctorReg.Register(&mockTool{name: "search"})

	n := New("n1", nil, ctorReg, Config{ToolNames: []string{"search"}})
	gotReg, gotAllow := n.resolveTools(graph.ExecutionContext{})

	if gotReg != ctorReg {
		t.Errorf("registry: expected constructor fallback, got %p want %p", gotReg, ctorReg)
	}
	if !reflect.DeepEqual(gotAllow, []string{"search"}) {
		t.Errorf("allow list: got %v want [search]", gotAllow)
	}
}

// TestNode_ResolveTools_AllowedNamesIntersection asserts the policy
// gate: the run-level ceiling (agent.Agent.Tools, promoted by
// agent.Run into depname.ToolAllowedNames) intersects with the
// per-node Config.ToolNames. Tools the agent does not authorise
// MUST NOT reach the LLM call even when the node config asks for
// them.
//
// Closes contract-audit #1 ("Agent.Tools is silently ignored") at
// the gate site: the helper is the only consumer of the allow-list
// in the LLM round and the only place the gate can take effect.
func TestNode_ResolveTools_AllowedNamesIntersection(t *testing.T) {
	deps := &engine.Dependencies{}
	deps.Set(depname.ToolAllowedNames, []string{"search", "fetch"})

	n := New("n1", nil, tool.NewRegistry(), Config{
		ToolNames: []string{"search", "delete_world", "fetch"},
	})
	_, gotAllow := n.resolveTools(graph.ExecutionContext{Deps: deps})

	sort.Strings(gotAllow)
	want := []string{"fetch", "search"}
	if !reflect.DeepEqual(gotAllow, want) {
		t.Errorf("intersection: got %v want %v (delete_world MUST be filtered out)", gotAllow, want)
	}
}

// TestNode_ResolveTools_EmptyCeilingDeniesAll documents the
// fail-closed semantics: an empty []string under ToolAllowedNames
// is a deliberate "no tools permitted" signal (e.g. an agent the
// operator restricted), distinct from "no key set" which falls
// back to legacy behaviour.
func TestNode_ResolveTools_EmptyCeilingDeniesAll(t *testing.T) {
	deps := &engine.Dependencies{}
	deps.Set(depname.ToolAllowedNames, []string{})

	n := New("n1", nil, tool.NewRegistry(), Config{ToolNames: []string{"search"}})
	_, gotAllow := n.resolveTools(graph.ExecutionContext{Deps: deps})

	if len(gotAllow) != 0 {
		t.Errorf("empty ceiling should deny all, got %v", gotAllow)
	}
}

// TestNode_ResolveTools_AbsentCeilingFallsBackToConfig confirms the
// legacy back-compat path. Engines that haven't been migrated to
// declare ToolAllowedNames (vessel inline engine pre-Epic-D) MUST
// keep working with the per-node ToolNames as the only filter.
func TestNode_ResolveTools_AbsentCeilingFallsBackToConfig(t *testing.T) {
	deps := &engine.Dependencies{}
	// Deps container present but no ToolAllowedNames key set.

	n := New("n1", nil, tool.NewRegistry(), Config{ToolNames: []string{"search", "fetch"}})
	_, gotAllow := n.resolveTools(graph.ExecutionContext{Deps: deps})

	want := []string{"search", "fetch"}
	if !reflect.DeepEqual(gotAllow, want) {
		t.Errorf("absent ceiling: got %v want %v (legacy fallback)", gotAllow, want)
	}
}

// TestNode_ResolveTools_EmptyConfigToolNamesIsDenyAll is the
// symmetric companion to EmptyCeilingDeniesAll: a node with no
// ToolNames opts out of tools regardless of the run-level
// ceiling. Prevents accidental tool exposure when a graph author
// left ToolNames empty by mistake.
func TestNode_ResolveTools_EmptyConfigToolNamesIsDenyAll(t *testing.T) {
	deps := &engine.Dependencies{}
	deps.Set(depname.ToolAllowedNames, []string{"search"})

	n := New("n1", nil, tool.NewRegistry(), Config{ToolNames: nil})
	_, gotAllow := n.resolveTools(graph.ExecutionContext{Deps: deps})

	if len(gotAllow) != 0 {
		t.Errorf("empty Config.ToolNames should deny all, got %v", gotAllow)
	}
}
