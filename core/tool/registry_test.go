package tool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// TestConflictPolicies pins the two answers to a duplicate name: refuse the second
// source, or let it win in place. A registry is built from a deployment's sources, so
// the choice is a deployment decision, not a surprise.
func TestConflictPolicies(t *testing.T) {
	first := namedTool("dup", "first")
	second := namedTool("dup", "second")

	if _, err := tool.NewRegistry([]tool.Source{sourceOf(first, second)}); err == nil {
		t.Fatal("the default policy accepted a duplicate name")
	}

	registry, err := tool.NewRegistry([]tool.Source{sourceOf(first, second)},
		tool.WithConflictPolicy(tool.ConflictOverwrite))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, ok := registry.Get("dup")
	if !ok {
		t.Fatal("the tool is missing")
	}
	if text, err := got.Execute(context.Background(), tool.Binding{}, ""); err != nil || text.Text() != "second" {
		t.Fatalf("the later source did not win: %q (%v)", text.Text(), err)
	}
	if defs := registry.Definitions(); len(defs) != 1 {
		t.Fatalf("definitions = %d, want 1: overwriting must not duplicate the name", len(defs))
	}
}

// TestDefinitionsAreSortedAndCached pins the two properties a projection and a prompt
// builder rely on: a stable order, and a cache that an add invalidates.
func TestDefinitionsAreSortedAndCached(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{sourceOf(
		namedTool("c", "c"), namedTool("a", "a"), namedTool("b", "b"))})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defs := registry.Definitions()
	if len(defs) != 3 || defs[0].Name != "a" || defs[2].Name != "c" {
		t.Fatalf("definitions = %v, want a,b,c", namesOf(defs))
	}
	if again := registry.Definitions(); len(again) != 3 {
		t.Fatalf("second call = %d", len(again))
	}
	if err := registry.Add(namedTool("d", "d")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if defs := registry.Definitions(); len(defs) != 4 {
		t.Fatalf("definitions after Add = %d, want 4", len(defs))
	}
}

// TestDefinitionDecoratorIsTheCatalogHalf pins the split the Catalog doc promises:
//
//   - Definitions carries the deployment's override, because that is what the model reads.
//   - Get returns the tool itself, undecorated, because that is what executes.
//
// A reader that re-derived the list from Get alone would silently undo the override, and
// this test is what says so out loud.
func TestDefinitionDecoratorIsTheCatalogHalf(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{sourceOf(namedTool("t", "t"))},
		tool.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
			def.Description = "decorated"
			return def
		}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, def := range registry.Definitions() {
		if def.Description != "decorated" {
			t.Fatalf("description = %q, want the decorated text", def.Description)
		}
	}
	got, ok := registry.Get("t")
	if !ok {
		t.Fatal("the tool is missing")
	}
	if desc := got.Definition().Description; desc != "t" {
		t.Fatalf("Get returned %q: the executable's own definition is the undecorated one", desc)
	}
	if names := namesOf(registry.Definitions()); len(names) != 1 || names[0] != "t" {
		t.Fatalf("decorating changed the name set: %v", names)
	}
}

// TestAddAfterCloseIsRefused keeps a closed registry closed: a tool registered into it
// could never be dispatched.
func TestAddAfterCloseIsRefused(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{sourceOf(namedTool("a", "a"))})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := registry.Add(namedTool("b", "b")); err == nil {
		t.Fatal("a closed registry accepted a tool")
	}
	registry.Remove("a") // must not panic on a closed registry
}

// TestLazyToolNeedsAreDeclaredUpFront: registration happens before the first call, so a
// proxy that looked cheaper than its tool would be accepted by a process that cannot
// serve it.
func TestLazyToolNeedsAreDeclaredUpFront(t *testing.T) {
	spec := tool.LazyTool{
		Name:        "lazy",
		Placeholder: namedTool("lazy", "x").Definition(),
		Load: func(context.Context) (tool.Tool, error) {
			return namedTool("lazy", "loaded"), nil
		},
	}
	if _, err := tool.NewRegistry([]tool.Source{lazyOnly{spec}}); err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	broken := spec
	broken.Load = nil
	if err := broken.Validate(); err == nil {
		t.Fatal("a LazyTool without a loader validated")
	}
	misnamed := spec
	misnamed.Name = "other"
	if err := misnamed.Validate(); err == nil {
		t.Fatal("a LazyTool whose name disagrees with its placeholder validated")
	}
}

// TestALoadFailureIsReportedOnceAndKept keeps a broken deferred tool from retrying the
// network on every round.
func TestALoadFailureIsReportedOnceAndKept(t *testing.T) {
	loads := 0
	registry, err := tool.NewRegistry([]tool.Source{lazyOnly{tool.LazyTool{
		Name:        "flaky",
		Placeholder: namedTool("flaky", "x").Definition(),
		Load: func(context.Context) (tool.Tool, error) {
			loads++
			return nil, errLazy
		},
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	one, ok := registry.Get("flaky")
	if !ok {
		t.Fatal("the lazy tool is missing")
	}
	for round := 0; round < 3; round++ {
		if _, err := one.Execute(context.Background(), tool.Binding{}, ""); !errors.Is(err, errLazy) {
			t.Fatalf("round %d: error = %v, want the loader's", round, err)
		}
	}
	if loads != 1 {
		t.Fatalf("the loader ran %d times, want once", loads)
	}
}

var errLazy = errors.New("lazy load failed")

func namedTool(name, text string) tool.Tool {
	return tool.TextTool(message.ToolDefinition{
		Name:        name,
		Description: name,
		InputSchema: []byte(`{"type":"object"}`),
	}, func(context.Context, string) (string, error) { return text, nil })
}

func sourceOf(tools ...tool.Tool) tool.Source { return fixedSource(tools) }

type fixedSource []tool.Tool

func (s fixedSource) Tools() []tool.Tool         { return s }
func (s fixedSource) LazyTools() []tool.LazyTool { return nil }

type lazyOnly struct{ one tool.LazyTool }

func (l lazyOnly) Tools() []tool.Tool         { return nil }
func (l lazyOnly) LazyTools() []tool.LazyTool { return []tool.LazyTool{l.one} }

func namesOf(defs []message.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Name)
	}
	return out
}
