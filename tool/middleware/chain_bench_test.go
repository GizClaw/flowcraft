package middleware_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/tool/dispatch"
	"github.com/GizClaw/flowcraft/tool/middleware"
)

// BenchmarkExecuteWithChain measures the same call through the middleware a deployment usually
// declares, so the per-layer cost has a number. The chain is the middleware package's own subject, so
// the fixture lives here rather than in dispatch's tests.
func BenchmarkExecuteWithChain(b *testing.B) {
	executor, binding := benchExecutor(b,
		middleware.Recover(),
		middleware.Telemetry(),
		middleware.Timeout(5*time.Second, nil),
	)
	call := benchCall()
	b.ReportAllocs()
	for b.Loop() {
		if res := executor.Execute(context.Background(), binding, call); res.IsError {
			b.Fatalf("Execute: %s", res.Content.Text())
		}
	}
}

// benchExecutor is one tool, one session, and whatever chain the case asks for.
func benchExecutor(b *testing.B, mws ...tool.Middleware) (*dispatch.Executor, tool.Binding) {
	b.Helper()
	echo := tool.TextTool(message.ToolDefinition{
		Name:        "echo",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(_ context.Context, args string) (string, error) { return args, nil })
	registry, err := tool.NewRegistry([]tool.Source{benchSource{echo}})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	return dispatch.New(registry, mws...), tool.Bind(benchSession{}, policy.Grant{})
}

func benchCall() message.ToolCall {
	return message.ToolCall{
		ID:        "call-1",
		Name:      "echo",
		Arguments: message.NewArguments([]byte(`{"text":"x"}`)),
	}
}

type benchSource struct{ tool.Tool }

func (s benchSource) Tools() []tool.Tool       { return []tool.Tool{s.Tool} }
func (benchSource) LazyTools() []tool.LazyTool { return nil }

type benchSession struct{}

func (benchSession) ID() string            { return "bench" }
func (benchSession) Scratch() tool.Scratch { return benchScratch{} }

type benchScratch struct{}

func (benchScratch) Load(string) (any, bool) { return nil, false }
func (benchScratch) Store(string, any)       {}
func (benchScratch) Delete(string)           {}
