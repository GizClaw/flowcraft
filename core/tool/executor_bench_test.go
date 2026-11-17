package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/GizClaw/flowcraft/core/tool/middleware"
)

// benchRecorder is a recorder that does nothing, so the benchmark measures the
// dispatch path and not a growing slice.
type benchRecorder struct{}

func (benchRecorder) RecordCall(message.ToolCall) {}

// BenchmarkExecutorExecute is the control for the context domains: one dispatch
// through the executor, which is the floor every host pays before its own
// middleware. Each call installs tool.Call — the id and the classification box the
// fold merged into one value — and the scope is read per call by the recorder
// middleware. The numbers are a baseline to compare against, not a target: the
// point is that a change to either domain shows up here.
func BenchmarkExecutorExecute(b *testing.B) {
	registry, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(message.ToolDefinition{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
			func(context.Context, string) (string, error) { return "ok", nil }),
	}}})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	call := message.ToolCall{ID: "call-1", Name: "echo",
		Arguments: message.NewArguments([]byte(`{}`))}

	bare := tool.NewExecutor(registry)
	// The shape an assembly runs: the telemetry middleware reads the call's box,
	// and the assembly's own recorder middleware reads the run's scope. The
	// deprecated RecordCalls is not it — an assembly appends that middleware
	// itself.
	assembly, err := engine.NewAssembly([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(message.ToolDefinition{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
			func(context.Context, string) (string, error) { return "ok", nil }),
	}}},
		// The assembly appends its own scope recorder when dynamic injection is
		// on; the telemetry entry is what a host usually adds on top.
		engine.WithMiddleware(middleware.Telemetry()),
		engine.WithDynamic(engine.Policy{Default: engine.ExposureAlways}))
	if err != nil {
		b.Fatalf("NewAssembly: %v", err)
	}
	instrumented := assembly.Dispatcher()
	scoped := tool.WithScope(context.Background(),
		tool.Scope{Recorder: benchRecorder{}})

	b.Run("bare", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if res := bare.Execute(context.Background(), call); res.IsError {
				b.Fatalf("result = %+v", res)
			}
		}
	})

	b.Run("instrumented", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if res := instrumented.Execute(scoped, call); res.IsError {
				b.Fatalf("result = %+v", res)
			}
		}
	})

	b.Run("instrumented/parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if res := instrumented.Execute(scoped, call); res.IsError {
					b.Fatalf("result = %+v", res)
				}
			}
		})
	})
}
