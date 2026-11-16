package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

// decoration counts how many definitions it rewrote, so the tests can tell one
// pass per rebuild from one pass per round.
type decoration struct {
	seen int
}

func (d *decoration) apply(def message.ToolDefinition) message.ToolDefinition {
	d.seen++
	def.Description = strings.ToUpper(def.Description)
	return def
}

func namedTool(name string) Tool {
	return FuncTool(message.ToolDefinition{
		Name:        name,
		Description: "tool " + name,
		InputSchema: []byte(`{"type": "object", "properties": {}}`),
	}, func(context.Context, string) (message.Content, error) {
		return message.NewTextContent("ok"), nil
	})
}

func staticSource(tools ...Tool) Source { return staticTools{tools: tools} }

type staticTools struct{ tools []Tool }

func (s staticTools) Tools() []Tool       { return s.tools }
func (staticTools) LazyTools() []LazyTool { return nil }

func TestRegistry_DecoratorRewritesOnlyTheModelFacingText(t *testing.T) {
	mark := &decoration{}
	registry, err := NewRegistry(nil, WithDefinitionDecorator(mark.apply))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(namedTool("alpha")); err != nil {
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
	registry, err := NewRegistry(nil, WithDefinitionDecorator(mark.apply))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(namedTool("alpha")); err != nil {
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
	if err := registry.Add(namedTool("beta")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defs := registry.Definitions()
	if len(defs) != 2 || defs[0].Description != "TOOL ALPHA" || defs[1].Description != "TOOL BETA" {
		t.Errorf("Definitions after a late Add = %+v, want both decorated", defs)
	}
}

func TestRegistry_IdentityDecoratorLeavesDefinitionsAlone(t *testing.T) {
	plain, err := NewRegistry([]Source{staticSource(namedTool("alpha"))})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	identity, err := NewRegistry([]Source{staticSource(namedTool("alpha"))},
		WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
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
// deployment that looks a tool up from its decorator must not deadlock, because a
// deadlock here hangs the loop rather than failing it.
func TestRegistry_DecoratorMayReadTheRegistry(t *testing.T) {
	var registry *Registry
	var err error
	registry, err = NewRegistry(nil, WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
		if _, ok := registry.Get(def.Name); !ok {
			t.Errorf("decorator cannot see %q", def.Name)
		}
		return def
	}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(namedTool("alpha")); err != nil {
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
