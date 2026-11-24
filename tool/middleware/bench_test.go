package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/dispatch"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/goccy/go-json"
)

// benchNoopTool is the terminal every benchmark wraps: it accepts any arguments
// and returns fixed content, so the measured time belongs to the chain.
type benchNoopTool struct{ def message.ToolDefinition }

func (t benchNoopTool) Definition() message.ToolDefinition { return t.def }

func (benchNoopTool) Needs() []tool.Need { return nil }

func (benchNoopTool) Execute(ctx context.Context, _ tool.Binding, arguments string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

// benchChain builds the per-call path a deployment gets: the default chain's
// recover and telemetry, a catalog-backed timeout, and the rate-limit lookup.
// The tool declares no rate limit, so the benchmark covers the steady state
// every call pays — a cache hit, not a timer wait — and the timeout is long
// enough that only its context wrapper is measured.
func benchChain(b *testing.B) (*dispatch.Executor, tool.Binding, message.ToolCall) {
	b.Helper()
	registry, err := tool.NewRegistry([]tool.Source{tooltest.Source(benchNoopTool{
		def: message.ToolDefinition{Name: "noop", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	exec := dispatch.New(registry,
		Recover(),
		Telemetry(),
		TimeoutWithCatalog(registry, 30*time.Second, nil),
		RateLimit(registry),
	)
	return exec, tool.Binding{Grant: policy.Grant{}, Session: tooltest.Session("bench")}, message.ToolCall{
		ID:        "call-1",
		Name:      "noop",
		Arguments: message.NewArguments(json.RawMessage(`{"x":1}`)),
	}
}

// BenchmarkChainExecute is the floor every tool call pays before the tool does
// any work of its own: one pass through recover, telemetry, the timeout wrapper
// and the two catalog caches.
func BenchmarkChainExecute(b *testing.B) {
	exec, caps, call := benchChain(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if res := exec.Execute(ctx, caps, call); res.IsError {
			b.Fatalf("execute: %s", res.Content.Text())
		}
	}
}

// BenchmarkChainExecuteParallel is the contention axis: one session already runs
// calls concurrently through ExecuteAll, and several sessions run them all the
// time, so the caches on the per-call path are read by every goroutine at once.
func BenchmarkChainExecuteParallel(b *testing.B) {
	exec, caps, call := benchChain(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if res := exec.Execute(ctx, caps, call); res.IsError {
				// FailNow is not allowed off the benchmark goroutine.
				b.Errorf("execute: %s", res.Content.Text())
				return
			}
		}
	})
}
