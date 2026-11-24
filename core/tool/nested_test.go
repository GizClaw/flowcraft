package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// TestNestedSlotRoundTrips pins the capability's identity: the host provides one
// implementation and a declaring tool reads it back as the interface, so the two sides
// agree without sharing a concrete type.
func TestNestedSlotRoundTrips(t *testing.T) {
	if name := tool.NestedSlot.Name(); name != "tool.nested" {
		t.Fatalf("slot name = %q", name)
	}
	var provided tool.Nested = &stubNested{}
	set := tool.NewCapSet()
	tool.NestedSlot.Provide(set, provided)

	got, ok := tool.NestedSlot.Get(set.View())
	if !ok || got == nil {
		t.Fatalf("Get = (%v, %v)", got, ok)
	}
	if res := got.Dispatch(context.Background(), message.ToolCall{ID: "c1", Name: "x"}); res.IsError {
		t.Fatalf("Dispatch: %s", res.Content.Text())
	}
}

// TestNestedIsRefusedAtRegistrationWhenTheProcessHasNone keeps the construction-time gate
// meaningful for this capability too: a process that cannot dispatch nested calls must not
// accept a tool that depends on them.
func TestNestedIsRefusedAtRegistrationWhenTheProcessHasNone(t *testing.T) {
	delegate := tool.CapFunc([]tool.Need{tool.NestedSlot}, definition("delegate"),
		func(context.Context, tool.Binding, string) (message.Content, error) {
			return message.NewTextContent("ok"), nil
		})
	if _, err := tool.NewRegistry([]tool.Source{sourceOf(delegate)}); err == nil {
		t.Fatal("a process with no nested dispatcher accepted a tool that needs one")
	}

	set := tool.NewCapSet()
	tool.NestedSlot.Provide(set, &stubNested{})
	if _, err := tool.NewRegistry([]tool.Source{sourceOf(delegate)}, tool.WithProvisioned(set)); err != nil {
		t.Fatalf("a process that provides the dispatcher refused the tool: %v", err)
	}
}

type stubNested struct{}

func (stubNested) Dispatch(_ context.Context, request message.ToolCall) message.ToolResult {
	return message.NewToolResult(request.ID, message.NewTextContent("nested"))
}
