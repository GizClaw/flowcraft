package tool_test

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestCallIDContext_RoundTrip(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call-42")
	if id, ok := tool.CallIDFromContext(ctx); !ok || id != "call-42" {
		t.Fatalf("CallIDFromContext = %q, %v; want call-42, true", id, ok)
	}
	if _, ok := tool.CallIDFromContext(context.Background()); ok {
		t.Fatal("CallIDFromContext outside a call should report absent")
	}
	if _, ok := tool.CallIDFromContext(tool.WithCallID(context.Background(), "")); ok {
		t.Fatal("empty call id should report absent")
	}
}

// TestExecutor_ExecuteStampsCallID verifies the executor stamps the
// active message.ToolCall.ID into the context handed to the tool
// implementation and to middleware.
func TestExecutor_ExecuteStampsCallID(t *testing.T) {
	var toolSeen, middlewareSeen string
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.FuncTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(ctx context.Context, _ string) (string, error) {
				toolSeen, _ = tool.CallIDFromContext(ctx)
				return "ok", nil
			},
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	mw := func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			middlewareSeen, _ = tool.CallIDFromContext(ctx)
			return next(ctx, call)
		}
	}
	exec := tool.NewExecutor(reg, mw)
	res := exec.Execute(context.Background(),
		message.ToolCall{ID: "call-7", Name: "peek", Arguments: []byte(`{}`)})
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if toolSeen != "call-7" {
		t.Fatalf("tool saw call id %q, want call-7", toolSeen)
	}
	if middlewareSeen != "call-7" {
		t.Fatalf("middleware saw call id %q, want call-7", middlewareSeen)
	}
}

// TestExecutor_ExecuteAllStampsPerCallIDs verifies concurrent calls
// each see their own call id.
func TestExecutor_ExecuteAllStampsPerCallIDs(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.FuncTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(ctx context.Context, _ string) (string, error) {
				id, _ := tool.CallIDFromContext(ctx)
				return id, nil
			},
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	exec := tool.NewExecutor(reg)
	calls := []message.ToolCall{
		{ID: "a", Name: "peek", Arguments: []byte(`{}`)},
		{ID: "b", Name: "peek", Arguments: []byte(`{}`)},
		{ID: "c", Name: "peek", Arguments: []byte(`{}`)},
	}
	results := exec.ExecuteAll(context.Background(), calls)
	var mu sync.Mutex
	got := make(map[string]bool, len(calls))
	for _, res := range results {
		if res.IsError {
			t.Fatalf("result = %+v", res)
		}
		mu.Lock()
		got[res.Content] = true
		mu.Unlock()
	}
	for _, call := range calls {
		if !got[call.ID] {
			t.Fatalf("call id %q not seen by any concurrent execution", call.ID)
		}
	}
}
