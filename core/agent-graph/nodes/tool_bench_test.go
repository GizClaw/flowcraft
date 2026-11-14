package nodes_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// benchToolSource answers instantly: the point of this benchmark is the node's
// per-call overhead, not the tool behind it.
type benchToolSource struct{ answer tool.Tool }

func (s benchToolSource) Tools() []tool.Tool { return []tool.Tool{s.answer} }

func (benchToolSource) LazyTools() []tool.LazyTool { return nil }

func benchToolRuntime(b *testing.B) *runtime.Runtime {
	b.Helper()
	answer := tool.FuncTool(message.ToolDefinition{
		Name:        "echo",
		Description: "answers with a fixed string",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, string) (message.Content, error) {
		return message.NewTextContent("ok"), nil
	})
	root := b.TempDir()
	config := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
	}
	rt, err := runtime.New(context.Background(), config,
		runtime.WithToolSource(benchToolSource{answer: answer}),
		runtime.WithDrivers(runtime.Driver{
			Name: "stub",
			Build: func(json.RawMessage) (inference.Provider, error) {
				// The runtime insists on a generator; this benchmark never calls one.
				return inference.Provider{Generator: &stubProvider{}}, nil
			},
		}))
	if err != nil {
		b.Fatalf("runtime.New: %v", err)
	}
	b.Cleanup(func() { _ = rt.Close() })
	return rt
}

// BenchmarkToolNodeCalls separates the node's fixed cost from its per-call cost:
// the node starts one goroutine per call, so a batch of one pays for machinery a
// batch of four amortises.
func BenchmarkToolNodeCalls(b *testing.B) {
	for _, calls := range []int{1, 4} {
		b.Run(fmt.Sprintf("calls=%d", calls), func(b *testing.B) {
			rt := benchToolRuntime(b)
			kind := nodes.Tool(rt)
			pending := make([]message.ToolCall, calls)
			for i := range pending {
				call, err := message.NewToolCall(fmt.Sprintf("c-%d", i), "echo", map[string]any{"n": i})
				if err != nil {
					b.Fatalf("NewToolCall: %v", err)
				}
				pending[i] = call
			}
			encoded, err := json.Marshal(pending)
			if err != nil {
				b.Fatalf("marshal pending: %v", err)
			}
			state := map[string]graph.Value{
				"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
					message.NewTextMessage(message.RoleUser, "hi"),
				})),
				"pending": graph.JSON(encoded),
			}
			invoke := graph.Invoke{
				Node:     "tools",
				Config:   json.RawMessage(`{"messages":"messages","pending":"pending"}`),
				State:    func(key string) graph.Value { return state[key] },
				Snapshot: func() graph.Snapshot { return graph.NewSnapshot(state) },
			}
			// The run context carries the catalog view, attached once per run by
			// host.Session; a benchmark without it would pay a view construction
			// per invocation that a real node invocation never pays.
			ctx := runtime.WithCatalogViewCtx(context.Background(), rt.NewCatalogView())
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				stream, err := kind.Invoke(ctx, invoke)
				if err != nil {
					b.Fatalf("Invoke: %v", err)
				}
				for {
					_, nextErr := stream.Next(ctx)
					if errors.Is(nextErr, io.EOF) {
						break
					}
					if nextErr != nil {
						b.Fatalf("Next: %v", nextErr)
					}
				}
				if _, err := stream.Result(); err != nil {
					b.Fatalf("Result: %v", err)
				}
			}
		})
	}
}
