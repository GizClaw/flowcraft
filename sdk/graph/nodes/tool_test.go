package nodes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// The tool node tests use the tool package's real components — FuncTool
// over a Registry over an Executor — the same shape host programs wire.

func toolRegistry(t *testing.T, dispatcher tool.Dispatcher) *graph.Registry {
	t.Helper()
	reg := graph.NewRegistry()
	if err := graph.RegisterType(reg, "tool", Tool(dispatcher)); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	return reg
}

func echoDispatcher() tool.Dispatcher {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		message.Definition{Name: "search", Description: "search the web"},
		func(_ context.Context, args string) (string, error) {
			return "results for " + args, nil
		},
	))
	return tool.NewExecutor(reg)
}

func assistantWithCalls(calls ...message.Call) message.Message {
	parts := make([]message.Part, len(calls))
	for i, call := range calls {
		parts[i] = message.ToolCallPart{Call: call}
	}
	return message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: parts},
	}
}

func TestToolNode_ExecutesAndAppendsToolMessage(t *testing.T) {
	reg := toolRegistry(t, echoDispatcher())
	g := singleNodeGraph(t, reg, "tool", ToolConfig{ResultsKey: "results"})

	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, assistantWithCalls(
		message.Call{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"weather"}`)},
		message.Call{ID: "call_2", Name: "search", Arguments: json.RawMessage(`{"q":"news"}`)},
	))
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 || msgs[1].Role != message.RoleTool {
		t.Fatalf("channel = %+v, want assistant + one role=tool message", msgs)
	}
	parts := msgs[1].Content.Parts
	if len(parts) != 2 {
		t.Fatalf("tool message parts = %d, want 2", len(parts))
	}
	// Model-issued call ids are preserved so the provider can pair
	// results on the next turn.
	for i, wantID := range []string{"call_1", "call_2"} {
		result, ok := parts[i].(message.ToolResultPart)
		if !ok {
			t.Fatalf("part %d = %T, want ToolResultPart", i, parts[i])
		}
		if result.Result.CallID != wantID || result.Result.Content == "" {
			t.Fatalf("result %d = %+v, want call %s with content", i, result.Result, wantID)
		}
	}

	v, ok := board.GetVar("results")
	if !ok {
		t.Fatal("results_key var missing")
	}
	results, ok := v.([]message.Result)
	if !ok || len(results) != 2 {
		t.Fatalf("results var = %T, want []message.Result len 2", v)
	}
}

func TestToolNode_RejectsBadTail(t *testing.T) {
	reg := toolRegistry(t, echoDispatcher())
	g := singleNodeGraph(t, reg, "tool", ToolConfig{})

	// Empty channel.
	if err := executeGraph(t, g, agent.NoopHost{}, agent.NewBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty channel error = %v, want validation-classified", err)
	}
	// Non-assistant tail.
	user := agent.NewBoard()
	user.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "hi"))
	if err := executeGraph(t, g, agent.NoopHost{}, user); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("user tail error = %v, want validation-classified", err)
	}
	// Assistant tail without tool calls.
	plain := agent.NewBoard()
	plain.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "done"))
	if err := executeGraph(t, g, agent.NoopHost{}, plain); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("no-tool-calls error = %v, want validation-classified", err)
	}
}

func TestToolNode_NoDispatcher(t *testing.T) {
	reg := toolRegistry(t, nil)
	g := singleNodeGraph(t, reg, "tool", ToolConfig{})
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, assistantWithCalls(
		message.Call{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{}`)},
	))
	if err := executeGraph(t, g, agent.NoopHost{}, board); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired dispatcher error = %v, want NotAvailable", err)
	}
}
