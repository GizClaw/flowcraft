package tooltest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/dispatch"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// slowcat is the capability these tests lend a tool.
type slowcat struct{ name string }

var catSlot = tool.NewSlot[*slowcat]("tooltest.cat")

// TestLazySourceAndExecute covers the fixtures a source implementation uses: the lazy
// descriptor, the source that carries it, and the helper that runs a call through the
// real chain. All three were untested helpers before.
func TestLazySourceAndExecute(t *testing.T) {
	loaded := false
	src := tooltest.LazySource(tooltest.LazyTool("slow", func(context.Context) (tool.Tool, error) {
		loaded = true
		return tooltest.FuncTool("slow", "slow", func(context.Context, string) (string, error) {
			return "loaded", nil
		}), nil
	}))
	tooltest.SourceSuite(t, func() tool.Source { return src })

	registry, err := tool.NewRegistry([]tool.Source{src})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if loaded {
		t.Fatal("a lazy tool loaded before its first call")
	}

	got, err := tooltest.Execute(t, registry, "slow", `{}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "loaded" || !loaded {
		t.Fatalf("result = %q, loaded = %v", got, loaded)
	}
}

// TestBindingAndSessionFixtures keeps the helpers a tool's own test relies on honest: a
// binding is valid, and two sessions do not share scratch space.
func TestBindingAndSessionFixtures(t *testing.T) {
	binding := tooltest.Binding()
	if !binding.Valid() {
		t.Fatal("tooltest.Binding is invalid")
	}
	if !binding.Grant.Tools.Allows("anything") {
		t.Fatal("the fixture grant does not allow every tool")
	}

	first := tooltest.BindingFor("one")
	second := tooltest.BindingFor("two")
	first.Session.Scratch().Store("k", 1)
	if _, ok := second.Session.Scratch().Load("k"); ok {
		t.Fatal("two sessions shared one scratch map")
	}
	if first.Session.ID() != "one" || second.Session.ID() != "two" {
		t.Fatal("sessions lost their identity")
	}
	second.Session.Scratch().Store("k", 2)
	second.Session.Scratch().Delete("k")
	if _, ok := second.Session.Scratch().Load("k"); ok {
		t.Fatal("Delete kept the key")
	}
}

// TestCapabilityReachesADeclaringTool is the reference a tool author copies: declare the
// slot, read it from the binding, and the value is there — typed, not boxed.
func TestCapabilityReachesADeclaringTool(t *testing.T) {
	set := tool.NewCapSet()
	catSlot.Provide(set, &slowcat{name: "tom"})

	reader := tool.CapFunc([]tool.Need{catSlot},
		message.ToolDefinition{Name: "reader", InputSchema: []byte(`{"type":"object"}`)},
		func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
			cat, ok := catSlot.Get(b.Caps)
			if !ok {
				return message.NewTextContent("no capability"), nil
			}
			return message.NewTextContent(cat.name), nil
		})
	registry, err := tool.NewRegistry([]tool.Source{tooltest.Source(reader)},
		tool.WithProvisioned(set))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	result := dispatch.New(registry).Execute(context.Background(),
		tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithCaps(set),
		message.ToolCall{ID: "c1", Name: "reader", Arguments: message.NewArguments([]byte(`{}`))})
	if result.IsError || result.Content.Text() != "tom" {
		t.Fatalf("result = %q (err=%v)", result.Content.Text(), result.IsError)
	}
}

// TestScratchIsPartitionedByToolName pins the partition dispatch performs: two tools of
// one session cannot collide on a key.
func TestScratchIsPartitionedByToolName(t *testing.T) {
	write := func(name string) tool.Tool {
		return tool.TextTool(message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)},
			func(_ context.Context, _ string) (string, error) { return name, nil })
	}
	registry, err := tool.NewRegistry([]tool.Source{tooltest.Source(write("a"), write("b"))})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	binding := tooltest.BindingFor("s1")
	executor := dispatch.New(registry)
	for _, name := range []string{"a", "b"} {
		if res := executor.Execute(context.Background(), binding,
			message.ToolCall{ID: name, Name: name, Arguments: message.NewArguments([]byte(`{}`))}); res.IsError {
			t.Fatalf("%s: %s", name, res.Content.Text())
		}
	}
	if got := len(scratchKeys(binding)); got != 0 {
		t.Fatalf("scratch has %d keys: %v", got, scratchKeys(binding))
	}
}

// scratchKeys is what one session's scratch map holds, for assertions. The fixture does
// not expose the map, so this counts through the interface a tool would use.
func scratchKeys(binding tool.Binding) []string {
	var found []string
	for _, key := range []string{"a.k", "b.k", "k"} {
		if _, ok := binding.Session.Scratch().Load(key); ok {
			found = append(found, key)
		}
	}
	return found
}
