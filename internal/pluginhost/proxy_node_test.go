package pluginhost

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

type mockNodeService struct {
	execFn func(ctx context.Context, nodeID string, config map[string]any, callbacks plugin.NodeCallbacks) (map[string]any, error)
}

func (m *mockNodeService) ListNodes(_ context.Context) ([]plugin.NodeSpec, error) { return nil, nil }
func (m *mockNodeService) Execute(ctx context.Context, nodeID string, config map[string]any, callbacks plugin.NodeCallbacks) (map[string]any, error) {
	return m.execFn(ctx, nodeID, config, callbacks)
}

func mockResolver(svc plugin.NodeServiceClient) NodeClientResolver {
	return func(_ string) (plugin.NodeServiceClient, error) {
		return svc, nil
	}
}

func TestProxyNode_ExecuteWithHostCallbacks(t *testing.T) {
	llmCalled := false
	toolCalled := false
	sandboxCalled := false

	host := &HostCallbackProvider{
		LLMGenerate: func(_ context.Context, prompt string) (string, error) {
			llmCalled = true
			return "llm:" + prompt, nil
		},
		ToolExecute: func(_ context.Context, name, args string) (string, error) {
			toolCalled = true
			return "tool:" + name, nil
		},
		SandboxExec: func(_ context.Context, command string) (string, error) {
			sandboxCalled = true
			return "exec:" + command, nil
		},
	}

	svc := &mockNodeService{
		execFn: func(ctx context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			llmResult, _ := cb.LLMGenerate(ctx, "hello")
			toolResult, _ := cb.ToolExecute(ctx, "calc", "{}")
			sandboxResult, _ := cb.SandboxExec(ctx, "ls")
			cb.StreamEmit("chunk")
			cb.SetVar("test_key", "test_value")
			return map[string]any{
				"llm":     llmResult,
				"tool":    toolResult,
				"sandbox": sandboxResult,
			}, nil
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), host)
	board := graph.NewBoard()

	execCtx := graph.ExecutionContext{Context: context.Background()}
	if err := node.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	if !llmCalled {
		t.Fatal("LLMGenerate callback not called")
	}
	if !toolCalled {
		t.Fatal("ToolExecute callback not called")
	}
	if !sandboxCalled {
		t.Fatal("SandboxExec callback not called")
	}

	if v, ok := board.GetVar("test_key"); !ok || v != "test_value" {
		t.Fatalf("expected test_value, got %v", v)
	}
	if v, ok := board.GetVar("llm"); !ok || v != "llm:hello" {
		t.Fatalf("expected llm:hello, got %v", v)
	}
}

func TestProxyNode_ExecuteNoHost(t *testing.T) {
	svc := &mockNodeService{
		execFn: func(ctx context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			_, err := cb.LLMGenerate(ctx, "test")
			if err == nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), nil)
	board := graph.NewBoard()

	execCtx2 := graph.ExecutionContext{Context: context.Background()}
	if err := node.ExecuteBoard(execCtx2, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
}

func TestProxyNode_NoResolver(t *testing.T) {
	node := NewProxyNode("n1", "custom", "p1", nil, nil, nil)
	board := graph.NewBoard()
	execCtx := graph.ExecutionContext{Context: context.Background()}
	err := node.ExecuteBoard(execCtx, board)
	if err == nil {
		t.Fatal("expected error when no resolver")
	}
}

func TestProxyNode_ResolverError(t *testing.T) {
	resolver := func(_ string) (plugin.NodeServiceClient, error) {
		return nil, fmt.Errorf("plugin not found")
	}

	node := NewProxyNode("n1", "custom", "p1", nil, resolver, nil)
	board := graph.NewBoard()
	execCtx := graph.ExecutionContext{Context: context.Background()}
	err := node.ExecuteBoard(execCtx, board)
	if err == nil {
		t.Fatal("expected error from resolver")
	}
	expected := `proxy_node "n1": resolve client: plugin not found`
	if err.Error() != expected {
		t.Fatalf("expected %q, got %q", expected, err.Error())
	}
}

func TestProxyNode_SignalCallback(t *testing.T) {
	signalCalled := false
	var capturedType string
	var capturedPayload any

	host := &HostCallbackProvider{
		Signal: func(_ context.Context, signalType string, payload any) error {
			signalCalled = true
			capturedType = signalType
			capturedPayload = payload
			return nil
		},
	}

	svc := &mockNodeService{
		execFn: func(ctx context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			return nil, cb.Signal(ctx, "stop_all", "urgent")
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), host)
	board := graph.NewBoard()

	execCtx3 := graph.ExecutionContext{Context: context.Background()}
	if err := node.ExecuteBoard(execCtx3, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if !signalCalled {
		t.Fatal("Signal callback not called")
	}
	if capturedType != "stop_all" {
		t.Fatalf("expected signal type 'stop_all', got %q", capturedType)
	}
	if capturedPayload != "urgent" {
		t.Fatalf("expected payload 'urgent', got %v", capturedPayload)
	}
}

func TestProxyNode_SignalNoHost(t *testing.T) {
	svc := &mockNodeService{
		execFn: func(ctx context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			err := cb.Signal(ctx, "test", nil)
			if err == nil {
				t.Fatal("expected error when signal host not configured")
			}
			return map[string]any{}, nil
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), nil)
	board := graph.NewBoard()
	execCtx := graph.ExecutionContext{Context: context.Background()}
	if err := node.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
}

func TestProxyNode_StreamEmitWithCallback(t *testing.T) {
	var captured []graph.StreamEvent

	svc := &mockNodeService{
		execFn: func(_ context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			cb.StreamEmit("hello")
			cb.StreamEmit("world")
			return map[string]any{}, nil
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), nil)
	board := graph.NewBoard()
	execCtx := graph.ExecutionContext{
		Context: context.Background(),
		Stream: func(ev graph.StreamEvent) {
			captured = append(captured, ev)
		},
	}
	if err := node.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("expected 2 stream events, got %d", len(captured))
	}
	if captured[0].Type != "plugin_stream" || captured[0].NodeID != "n1" || captured[0].Payload != "hello" {
		t.Fatalf("unexpected first event: %+v", captured[0])
	}
}

func TestProxyNode_StreamEmitNoCallback(t *testing.T) {
	svc := &mockNodeService{
		execFn: func(_ context.Context, _ string, _ map[string]any, cb plugin.NodeCallbacks) (map[string]any, error) {
			cb.StreamEmit("ignored")
			return map[string]any{}, nil
		},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, mockResolver(svc), nil)
	board := graph.NewBoard()
	execCtx := graph.ExecutionContext{Context: context.Background()}
	if err := node.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
}

func TestProxyNode_DefaultPorts(t *testing.T) {
	node := NewProxyNode("n1", "custom", "p1", nil, nil, nil)
	in := node.InputPorts()
	out := node.OutputPorts()
	if len(in) != 1 || in[0].Name != "input" || in[0].Type != graph.PortTypeAny {
		t.Fatalf("unexpected default input ports: %+v", in)
	}
	if len(out) != 1 || out[0].Name != "output" || out[0].Type != graph.PortTypeAny {
		t.Fatalf("unexpected default output ports: %+v", out)
	}
}

func TestProxyNode_WithPorts(t *testing.T) {
	customIn := []graph.Port{
		{Name: "query", Type: graph.PortTypeString, Required: true},
		{Name: "context", Type: graph.PortTypeMessages},
	}
	customOut := []graph.Port{
		{Name: "answer", Type: graph.PortTypeString},
	}

	node := NewProxyNode("n1", "custom", "p1", nil, nil, nil, WithPorts(customIn, customOut))
	if len(node.InputPorts()) != 2 || node.InputPorts()[0].Name != "query" {
		t.Fatalf("expected custom input ports, got %+v", node.InputPorts())
	}
	if len(node.OutputPorts()) != 1 || node.OutputPorts()[0].Name != "answer" {
		t.Fatalf("expected custom output ports, got %+v", node.OutputPorts())
	}
}
