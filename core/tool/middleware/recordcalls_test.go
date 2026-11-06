package middleware

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

func TestRecordCalls_FeedsSession(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{testSource{tools: []tool.Tool{echoTool("direct")}}},
		engine.WithDynamic(engine.Policy{
			Default:   engine.ExposureDeferred,
			Exposures: map[string]engine.Exposure{"direct": engine.ExposureDirect},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	//nolint:staticcheck // standalone deprecated middleware: covered for custom dispatchers
	exec := tool.NewExecutor(assembly.Catalog(), RecordCalls())
	session := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), session)

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
	assembly, err := engine.NewAssembly(
		[]tool.Source{testSource{tools: []tool.Tool{echoTool("direct")}}},
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	//nolint:staticcheck // standalone deprecated middleware: covered for custom dispatchers
	exec := tool.NewExecutor(assembly.Catalog(), RecordCalls())
	res := exec.Execute(context.Background(), call("direct"))
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
}
