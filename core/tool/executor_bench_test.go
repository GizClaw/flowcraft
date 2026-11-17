package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/GizClaw/flowcraft/core/tool/middleware"
)

// benchTool is the one tool every sub-benchmark dispatches, built per chain so the
// two chains cannot drift apart.
func benchTool(b testing.TB) tool.Tool {
	b.Helper()
	return tool.TextTool(
		message.ToolDefinition{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
		func(context.Context, string) (string, error) { return "ok", nil })
}

// BenchmarkExecutorExecute is the control for the context domains: one dispatch
// through the executor, which is the floor every host pays before its own
// middleware. Each call installs tool.Call — the id and the classification box the
// fold merged into one context value.
//
// Read each number against its own baseline; do not subtract them. "bare" is the
// chain with nothing on it, "instrumented" is the shape an assembly runs (telemetry
// middleware plus the assembly's own scope recorder, over the run's catalog view),
// and the two differ by a span option per call as much as by the context shape.
func BenchmarkExecutorExecute(b *testing.B) {
	call := message.ToolCall{ID: "call-1", Name: "echo",
		Arguments: message.NewArguments([]byte(`{}`))}

	registry, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{benchTool(b)}}})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	bare := tool.NewExecutor(registry)

	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{benchTool(b)}}},
		engine.WithMiddleware(middleware.Telemetry()),
		engine.WithDynamic(engine.Policy{Default: engine.ExposureAlways}),
	)
	if err != nil {
		b.Fatalf("NewAssembly: %v", err)
	}
	b.Cleanup(func() { _ = assembly.Close() })
	// The run's view, not a bare recorder: with a plain CallRecorder the engine
	// cannot narrow it, so the recorder middleware records nothing and the chain
	// stops being the one an assembly runs.
	scoped := engine.WithCatalogView(context.Background(), assembly.NewCatalogView())

	run := func(b *testing.B, exec tool.Dispatcher, ctx context.Context) {
		b.Helper()
		b.ReportAllocs()
		for b.Loop() {
			if res := exec.Execute(ctx, call); res.IsError {
				b.Fatalf("result = %+v", res)
			}
		}
	}

	b.Run("bare", func(b *testing.B) { run(b, bare, context.Background()) })
	b.Run("instrumented", func(b *testing.B) { run(b, assembly.Dispatcher(), scoped) })

	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		dispatcher := assembly.Dispatcher()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if res := dispatcher.Execute(scoped, call); res.IsError {
					// FailNow is not allowed off the benchmark goroutine; report
					// and stop this worker.
					b.Errorf("result = %+v", res)
					return
				}
			}
		})
	})
}
