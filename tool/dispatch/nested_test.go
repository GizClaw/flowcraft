package dispatch_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/tool/dispatch"
)

// nestedRecorder is the host's session-bound dispatcher: it records what it was asked to
// run, so the test can assert a nested call reached it exactly once.
type nestedRecorder struct{ calls []string }

func (n *nestedRecorder) Dispatch(_ context.Context, request message.ToolCall) message.ToolResult {
	n.calls = append(n.calls, request.Name)
	return message.NewToolResult(request.ID, message.NewTextContent("nested:"+request.Name))
}

// TestNestedCapabilityReachesOnlyDeclaringTools is the enforcement half of tool.Nested:
// through the real chain, a tool that declares NestedSlot receives the dispatcher and one
// that declares nothing cannot reach it. A plain tool must not be able to recurse into the
// toolset, which is the reason the capability is a slot rather than a field on the binding.
func TestNestedCapabilityReachesOnlyDeclaringTools(t *testing.T) {
	nested := &nestedRecorder{}
	set := tool.NewCapSet()
	tool.NestedSlot.Provide(set, nested)

	var declared, undeclared bool
	delegate := tool.CapFunc([]tool.Need{tool.NestedSlot}, definition("delegate"),
		func(ctx context.Context, b tool.Binding, _ string) (message.Content, error) {
			inner, ok := tool.NestedSlot.Get(b.Caps)
			declared = ok
			if ok {
				inner.Dispatch(ctx, message.ToolCall{ID: "inner", Name: "read_file"})
			}
			return message.NewTextContent("ok"), nil
		})
	plain := tool.CapFunc(nil, definition("plain"),
		func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
			_, undeclared = tool.NestedSlot.Get(b.Caps)
			return message.NewTextContent("ok"), nil
		})

	registry, err := tool.NewRegistry(nil, tool.WithProvisioned(set))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, impl := range []tool.Tool{delegate, plain} {
		if err := registry.Add(impl); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	executor := dispatch.New(registry)
	binding := bound(t).WithCaps(set)

	for _, name := range []string{"delegate", "plain"} {
		if res := executor.Execute(context.Background(), binding, request(name)); res.IsError {
			t.Fatalf("%s: %s", name, res.Content.Text())
		}
	}
	if !declared {
		t.Fatal("a tool that declared NestedSlot did not receive it")
	}
	if undeclared {
		t.Fatal("a tool that declared nothing reached the nested dispatcher")
	}
	if len(nested.calls) != 1 || nested.calls[0] != "read_file" {
		t.Fatalf("nested calls = %v, want [read_file]", nested.calls)
	}
}
