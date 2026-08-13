package middleware

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

func TestRecordCalls_FeedsSession(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{testSource{tools: []tool.Tool{echoTool("direct")}}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Exposures: map[string]tool.Exposure{"direct": tool.ExposureDirect},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	exec := tool.NewExecutor(assembly.Catalog(), RecordCalls())
	session := assembly.NewSession()
	ctx := tool.WithSession(context.Background(), session)

	exec.Execute(ctx, call("direct"))

	found := false
	for _, def := range session.Definitions() {
		if def.Name == "direct" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RecordCalls did not surface the called tool in the session")
	}
}

func TestRecordCalls_NoSessionIsNoOp(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{testSource{tools: []tool.Tool{echoTool("direct")}}},
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	exec := tool.NewExecutor(assembly.Catalog(), RecordCalls())
	res := exec.Execute(context.Background(), call("direct"))
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
}
