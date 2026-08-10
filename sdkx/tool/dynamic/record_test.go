package dynamic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

func TestRecordCalls_FeedsSessionCatalog(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("direct_tool", "x"))
	c := New(reg, WithExposure("direct_tool", ExposureDirect), WithSelectedRetention(2))
	t.Cleanup(func() { _ = c.Close() })

	exec := sdktool.NewExecutor(reg, RecordCalls())
	call := message.Call{ID: "c1", Name: "direct_tool", Arguments: []byte(`{}`)}
	if res := exec.Execute(WithCatalog(context.Background(), c), call); res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got := names(c.Definitions()); len(got) != 1 || got[0] != "direct_tool" {
		t.Errorf("Definitions after RecordCalls = %v, want [direct_tool]", got)
	}
}

func TestRecordCalls_NoCatalogIsNoop(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("x", "x"))
	exec := sdktool.NewExecutor(reg, RecordCalls())
	res := exec.Execute(context.Background(),
		message.Call{ID: "c1", Name: "x", Arguments: []byte(`{}`)})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
}
