package dispatch_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/tool/dispatch"
)

// The benchmarks here measure one dispatched call, which is the unit a model turn
// multiplies by the number of calls it makes. The chain benchmarks exist to keep the
// cost of adding a middleware visible: a chain that quietly allocates per layer is a
// chain a deployment pays for on every call.

func benchTool() tool.Tool {
	return tool.FuncTool(message.ToolDefinition{
		Name:        "echo",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(_ context.Context, args string) (message.Content, error) {
		return message.NewTextContent(args), nil
	})
}

func benchExecutor(b *testing.B, mws ...tool.Middleware) (*dispatch.Executor, tool.Binding) {
	b.Helper()
	registry, err := tool.NewRegistry([]tool.Source{staticSource{benchTool()}})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	binding := tool.Bind(benchSession{}, policy.Grant{})
	return dispatch.New(registry, mws...), binding
}

func benchCall() message.ToolCall {
	return message.ToolCall{
		ID:        "call-1",
		Name:      "echo",
		Arguments: message.NewArguments([]byte(`{"text":"x"}`)),
	}
}

// BenchmarkExecute is the floor: binding check, grant check, lookup, call.
func BenchmarkExecute(b *testing.B) {
	executor, binding := benchExecutor(b)
	call := benchCall()
	b.ReportAllocs()
	for b.Loop() {
		if res := executor.Execute(context.Background(), binding, call); res.IsError {
			b.Fatalf("Execute: %s", res.Content.Text())
		}
	}
}

// BenchmarkExecuteAll is the fan-out shape: a turn that emits several calls at once.
func BenchmarkExecuteAll(b *testing.B) {
	executor, binding := benchExecutor(b)
	calls := make([]message.ToolCall, 8)
	for i := range calls {
		calls[i] = benchCall()
		calls[i].ID = "call-" + string(rune('a'+i))
	}
	b.ReportAllocs()
	for b.Loop() {
		if results := executor.ExecuteAll(context.Background(), binding, calls); len(results) != len(calls) {
			b.Fatalf("results = %d", len(results))
		}
	}
}

// BenchmarkRefusal is the cheap path worth keeping cheap: a name the grant disallows never
// reaches a tool, and a model that guesses names should not cost a tool execution.
func BenchmarkRefusal(b *testing.B) {
	executor, _ := benchExecutor(b)
	binding := tool.Bind(benchSession{}, policy.Grant{Tools: policy.ToolSet{Allow: []string{"other"}}})
	call := benchCall()
	b.ReportAllocs()
	for b.Loop() {
		if res := executor.Execute(context.Background(), binding, call); !res.IsError {
			b.Fatal("a disallowed call was executed")
		}
	}
}

type staticSource []tool.Tool

func (s staticSource) Tools() []tool.Tool         { return s }
func (s staticSource) LazyTools() []tool.LazyTool { return nil }

type benchSession struct{}

func (benchSession) ID() string            { return "bench" }
func (benchSession) Scratch() tool.Scratch { return benchScratch{} }

type benchScratch struct{}

func (benchScratch) Load(string) (any, bool) { return nil, false }
func (benchScratch) Store(string, any)       {}
func (benchScratch) Delete(string)           {}
