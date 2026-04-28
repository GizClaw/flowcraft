package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func TestLocalExecutor_NodeRetry(t *testing.T) {
	attempts := 0
	g := buildGraph("test", "flaky",
		map[string]graph.Node{
			"flaky": newTestNode("flaky", func(_ graph.ExecutionContext, b *graph.Board) error {
				attempts++
				if attempts < 3 {
					return fmt.Errorf("transient error")
				}
				b.SetVar("done", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "flaky", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithMaxNodeRetries(5))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v, _ := result.GetVar("done"); v != true {
		t.Fatal("expected done=true after retries")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestLocalExecutor_StreamCallback_ToolCallCapture(t *testing.T) {
	node := newTestNode("llm", func(ctx graph.ExecutionContext, b *graph.Board) error {
		if ctx.Stream != nil {
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_call",
				NodeID: "llm",
				Payload: map[string]any{
					"id":        "tc-1",
					"name":      "web_search",
					"arguments": `{"q":"golang"}`,
				},
			})
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_call",
				NodeID: "llm",
				Payload: map[string]any{
					"id":        "tc-2",
					"name":      "code_run",
					"arguments": `{"code":"fmt.Println()"}`,
				},
			})
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_result",
				NodeID: "llm",
				Payload: map[string]any{
					"tool_call_id": "tc-1",
					"content":      "search result",
				},
			})
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_result",
				NodeID: "llm",
				Payload: map[string]any{
					"tool_call_id": "tc-2",
					"content":      "error output",
					"is_error":     true,
				},
			})
		}
		b.SetVar("answer", "done")
		return nil
	})

	g := buildGraph("test", "llm",
		map[string]graph.Node{"llm": node},
		[]graph.Edge{{From: "llm", To: graph.END}},
	)

	var captured []graph.StreamEvent
	cb := func(se graph.StreamEvent) {
		captured = append(captured, se)
	}

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board, WithStreamCallback(cb))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if len(captured) != 4 {
		t.Fatalf("expected 4 stream events, got %d", len(captured))
	}

	tcRaw, ok := result.GetVar(graph.VarToolCalls)
	if !ok {
		t.Fatal("expected VarToolCalls on board")
	}
	tc, ok := tcRaw.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", tcRaw)
	}
	if len(tc) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tc))
	}

	first := tc[0].(map[string]any)
	if first["name"] != "web_search" {
		t.Fatalf("expected web_search, got %v", first["name"])
	}
	if first["status"] != "success" {
		t.Fatalf("expected status success, got %v", first["status"])
	}
	if first["result"] != "search result" {
		t.Fatalf("expected 'search result', got %v", first["result"])
	}

	second := tc[1].(map[string]any)
	if second["name"] != "code_run" {
		t.Fatalf("expected code_run, got %v", second["name"])
	}
	if second["status"] != "error" {
		t.Fatalf("expected status error, got %v", second["status"])
	}
	if second["result"] != "error output" {
		t.Fatalf("expected 'error output', got %v", second["result"])
	}
}

func TestLocalExecutor_StreamCallback_NoToolCalls(t *testing.T) {
	node := newTestNode("simple", func(ctx graph.ExecutionContext, b *graph.Board) error {
		if ctx.Stream != nil {
			ctx.Stream(graph.StreamEvent{
				Type:    "token",
				NodeID:  "simple",
				Payload: map[string]any{"chunk": "hello"},
			})
		}
		b.SetVar("answer", "done")
		return nil
	})

	g := buildGraph("test", "simple",
		map[string]graph.Node{"simple": node},
		[]graph.Edge{{From: "simple", To: graph.END}},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board, WithStreamCallback(func(se graph.StreamEvent) {}))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if _, ok := result.GetVar(graph.VarToolCalls); ok {
		t.Fatal("expected no VarToolCalls for non-tool stream events")
	}
}

func TestLocalExecutor_StreamCallback_NilCallback(t *testing.T) {
	node := newTestNode("llm", func(ctx graph.ExecutionContext, b *graph.Board) error {
		if ctx.Stream != nil {
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_call",
				NodeID: "llm",
				Payload: map[string]any{
					"id": "tc-1", "name": "search", "arguments": "{}",
				},
			})
		}
		return nil
	})

	g := buildGraph("test", "llm",
		map[string]graph.Node{"llm": node},
		[]graph.Edge{{From: "llm", To: graph.END}},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	tcRaw, ok := board.GetVar(graph.VarToolCalls)
	if !ok {
		t.Fatal("expected VarToolCalls even without external callback")
	}
	tc := tcRaw.([]any)
	if len(tc) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tc))
	}
}
