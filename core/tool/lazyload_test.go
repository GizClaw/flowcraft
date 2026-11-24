package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func definition(name string) message.ToolDefinition {
	return message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)}
}

type lateCapability struct{}

var lateSlot = tool.NewSlot[*lateCapability]("test.late")

// TestLazyToolThatLoadsADifferentDeclarationIsRefused pins the declaration contract:
// dispatch derives a tool's capability allowance from the needs announced before
// loading, so a tool that only reveals a need afterwards would read that slot as
// absent for the rest of its life. The load fails and names the slot instead.
func TestLazyToolThatLoadsADifferentDeclarationIsRefused(t *testing.T) {
	undeclared := tool.TextTool(definition("late"), func(context.Context, string) (string, error) {
		return "ok", nil
	})
	registry, err := tool.NewRegistry([]tool.Source{lazySource{tool.LazyTool{
		Name:        "late",
		Placeholder: definition("late"),
		// Declares nothing, loads a tool that needs the capability.
		Load: func(context.Context) (tool.Tool, error) {
			return tool.CapFunc([]tool.Need{lateSlot}, definition("late"),
				func(context.Context, tool.Binding, string) (message.Content, error) {
					return message.NewTextContent("ok"), nil
				}), nil
		},
	}}}, tool.WithProvisioned(pocketFor(lateSlot)))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(undeclared); err == nil {
		t.Fatal("a duplicate name was accepted")
	}

	// Loading through EnsureLoaded is the only path that reveals the mismatch.
	one, ok := registry.Get("late")
	if !ok {
		t.Fatal("the lazy tool was not registered")
	}
	err = one.(interface{ EnsureLoaded(context.Context) error }).EnsureLoaded(context.Background())
	if err == nil {
		t.Fatal("a tool that loaded a need it did not declare was accepted")
	}
}

// TestLazyToolMayLoadFewerNeedsThanDeclared is the other direction: announcing more
// than the tool turns out to need costs nothing but an unread slot.
func TestLazyToolMayLoadFewerNeedsThanDeclared(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{lazySource{tool.LazyTool{
		Name:        "small",
		Placeholder: definition("small"),
		Needs:       []tool.Need{lateSlot},
		Load: func(context.Context) (tool.Tool, error) {
			return tool.TextTool(definition("small"), func(context.Context, string) (string, error) {
				return "ok", nil
			}), nil
		},
	}}}, tool.WithProvisioned(pocketFor(lateSlot)))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	one, ok := registry.Get("small")
	if !ok {
		t.Fatal("the lazy tool was not registered")
	}
	if err := one.(interface{ EnsureLoaded(context.Context) error }).EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
}

type lazySource struct{ one tool.LazyTool }

func (s lazySource) Tools() []tool.Tool         { return nil }
func (s lazySource) LazyTools() []tool.LazyTool { return []tool.LazyTool{s.one} }

func pocketFor(needs ...tool.Need) *tool.CapSet {
	set := tool.NewCapSet()
	for _, need := range needs {
		switch need.SlotName() {
		case "test.late":
			lateSlot.Provide(set, &lateCapability{})
		}
	}
	return set
}
