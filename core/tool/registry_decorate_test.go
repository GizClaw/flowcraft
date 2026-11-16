package tool_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// decoration counts how many definitions it rewrote, so a test can tell one pass
// per rebuild from one pass per round.
type decoration struct {
	seen int
}

func (d *decoration) apply(def message.ToolDefinition) message.ToolDefinition {
	d.seen++
	def.Description = strings.ToUpper(def.Description)
	return def
}

func describedTool(name, description string) tool.Tool {
	return tooltest.FuncTool(name, description, func(context.Context, string) (string, error) {
		return "ok", nil
	})
}

func TestRegistry_DecoratorRewritesOnlyTheModelFacingText(t *testing.T) {
	mark := &decoration{}
	registry, err := tool.NewRegistry(nil, tool.WithDefinitionDecorator(mark.apply))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(describedTool("alpha", "tool alpha")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defs := registry.Definitions()
	if len(defs) != 1 || defs[0].Description != "TOOL ALPHA" {
		t.Fatalf("Definitions = %+v, want the decorated description", defs)
	}
	// The tool itself still says what its author wrote: decoration is prompt
	// surface, not the tool contract.
	registered, ok := registry.Get("alpha")
	if !ok {
		t.Fatal("Get(alpha) = false")
	}
	if got := registered.Definition().Description; got != "tool alpha" {
		t.Errorf("Get(alpha).Definition = %q, want the undecorated text", got)
	}
}

// Definitions are cached, so a decorator runs once per rebuild — not once per
// round, which is how often the loop asks for them.
func TestRegistry_DecoratorRunsOncePerRebuild(t *testing.T) {
	mark := &decoration{}
	registry, err := tool.NewRegistry(nil, tool.WithDefinitionDecorator(mark.apply))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(describedTool("alpha", "tool alpha")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	registry.Definitions()
	first := mark.seen
	registry.Definitions()
	if mark.seen != first {
		t.Errorf("decorator ran %d times for two reads, want the cached count %d", mark.seen, first)
	}
	// A rebuild happens when the set can change, and the new tool is decorated
	// like the rest: this is the path a deferred source publishes through.
	if err := registry.Add(describedTool("beta", "tool beta")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defs := registry.Definitions()
	if len(defs) != 2 || defs[0].Description != "TOOL ALPHA" || defs[1].Description != "TOOL BETA" {
		t.Errorf("Definitions after a late Add = %+v, want both decorated", defs)
	}
}

// A deferred tool is served from its placeholder until it loads, then the load
// invalidates the cache and the real definition is decorated on the next rebuild.
// A decorator applied once to the placeholder would otherwise be the only trace of
// the override, and nothing else in the suite would notice.
func TestRegistry_DecoratorCoversADeferredLoad(t *testing.T) {
	registry, err := tool.NewRegistry(
		[]tool.Source{tooltest.LazySource(tooltest.LazyTool("deferred",
			func(context.Context) (tool.Tool, error) {
				return describedTool("deferred", "the loaded text"), nil
			}))},
		tool.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
			def.Description = "decorated " + def.Name
			return def
		}),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := descriptionOf(t, registry, "deferred"); got != "decorated deferred" {
		t.Fatalf("placeholder description = %q", got)
	}
	lazy, ok := registry.Get("deferred")
	if !ok {
		t.Fatal("Get(deferred) = false")
	}
	if _, err := lazy.Execute(context.Background(), "{}"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := descriptionOf(t, registry, "deferred"); got != "decorated deferred" {
		t.Errorf("loaded description = %q, want the decorated text", got)
	}
}

func TestRegistry_IdentityDecoratorLeavesDefinitionsAlone(t *testing.T) {
	plain, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{describedTool("alpha", "tool alpha")}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	identity, err := tool.NewRegistry(
		[]tool.Source{source{tools: []tool.Tool{describedTool("alpha", "tool alpha")}}},
		tool.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
			return def
		}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, want := identity.Definitions(), plain.Definitions()
	if len(got) != len(want) {
		t.Fatalf("Definitions = %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Description != want[i].Description ||
			string(got[i].InputSchema) != string(want[i].InputSchema) {
			t.Errorf("definition %d changed under an identity decorator", i)
		}
	}
}

// The decorator is caller code, and Definitions takes no lock while it runs: a
// deployment that mutates from its decorator must not deadlock, because a deadlock
// here hangs the loop rather than failing it.
//
// Remove is the probe rather than Get: a nested read lock succeeds whenever no
// writer is queued, so a decorator that only read would pass this test even if it
// ran inside the registry's own read lock.
func TestRegistry_DecoratorMayWriteToTheRegistry(t *testing.T) {
	var registry *tool.Registry
	var err error
	registry, err = tool.NewRegistry(nil, tool.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
		registry.Remove("absent") // documented no-op, and it takes the write lock
		return def
	}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(describedTool("alpha", "tool alpha")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		registry.Definitions()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Definitions did not return: the decorator ran under a held lock")
	}
}

func descriptionOf(t *testing.T, registry *tool.Registry, name string) string {
	t.Helper()
	for _, def := range registry.Definitions() {
		if def.Name == name {
			return def.Description
		}
	}
	t.Fatalf("%q is not in the definitions", name)
	return ""
}
