package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"

	pb "github.com/GizClaw/flowcraft/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestWithInfo(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	info := PluginInfo{ID: "p1", Name: "Plugin One", Version: "1.0.0", Type: TypeTool}
	WithInfo(info)(s)

	if s.info.ID != "p1" || s.info.Name != "Plugin One" {
		t.Errorf("WithInfo did not set info: %+v", s.info)
	}
}

func TestWithTool(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	handler := func(ctx context.Context, args string) (string, error) { return "ok", nil }
	schema := map[string]any{"type": "object"}

	WithTool("my_tool", "does stuff", schema, handler)(s)

	entry, ok := s.tools["my_tool"]
	if !ok {
		t.Fatal("tool not registered")
	}
	if entry.spec.Name != "my_tool" || entry.spec.Description != "does stuff" {
		t.Errorf("tool spec mismatch: %+v", entry.spec)
	}
	result, err := entry.handler(context.Background(), "")
	if err != nil || result != "ok" {
		t.Errorf("handler returned (%q, %v)", result, err)
	}
}

func TestWithNode(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	handler := func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) {
		return map[string]any{"out": "done"}, nil
	}

	WithNode("custom_node", nil, handler)(s)

	entry, ok := s.nodes["custom_node"]
	if !ok {
		t.Fatal("node not registered")
	}
	if entry.spec.Type != "custom_node" {
		t.Errorf("node type = %q, want %q", entry.spec.Type, "custom_node")
	}
}

func TestMultipleOptions(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	noop := func(ctx context.Context, args string) (string, error) { return "", nil }

	WithInfo(PluginInfo{ID: "multi", Name: "Multi", Version: "1.0.0", Type: TypeTool})(s)
	WithTool("t1", "tool 1", nil, noop)(s)
	WithTool("t2", "tool 2", nil, noop)(s)

	if len(s.tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(s.tools))
	}
	if s.info.ID != "multi" {
		t.Errorf("info.ID = %q, want %q", s.info.ID, "multi")
	}
}

// startTestServer boots a gRPC server over a Unix socket and returns a client connection.
func startTestServer(t *testing.T, s *server) *grpc.ClientConn {
	t.Helper()
	f, err := os.CreateTemp("", "fc-*.sock")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock)
	t.Cleanup(func() { os.Remove(sock) })

	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcSrv := grpc.NewServer()
	pb.RegisterPluginLifecycleServer(grpcSrv, &lifecycleServer{s: s})
	pb.RegisterToolServiceServer(grpcSrv, &toolServer{s: s})
	pb.RegisterNodeServiceServer(grpcSrv, &nodeServer{s: s})

	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient(
		"unix:"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestLifecycleHandshake(t *testing.T) {
	s := &server{
		tools: make(map[string]toolEntry),
		nodes: make(map[string]nodeEntry),
		info: PluginInfo{
			ID:          "lc-test",
			Name:        "Lifecycle Test",
			Version:     "0.1.0",
			Type:        TypeTool,
			Description: "test desc",
			Author:      "tester",
		},
	}
	noop := func(ctx context.Context, args string) (string, error) { return "", nil }
	WithTool("greet", "say hello", map[string]any{"type": "object"}, noop)(s)

	conn := startTestServer(t, s)
	client := pb.NewPluginLifecycleClient(conn)

	resp, err := client.Handshake(context.Background(), &pb.HandshakeRequest{
		ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	if resp.PluginInfo.Id != "lc-test" {
		t.Errorf("plugin id = %q, want %q", resp.PluginInfo.Id, "lc-test")
	}
	if resp.PluginInfo.Name != "Lifecycle Test" {
		t.Errorf("plugin name = %q", resp.PluginInfo.Name)
	}
	if resp.ProtocolVersion != 1 {
		t.Errorf("protocol version = %d, want 1", resp.ProtocolVersion)
	}
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "greet" {
		t.Errorf("tools = %v", resp.Tools)
	}
}

func TestLifecycleHandshakeWithNodes(t *testing.T) {
	s := &server{
		tools: make(map[string]toolEntry),
		nodes: make(map[string]nodeEntry),
		info:  PluginInfo{ID: "node-plugin", Name: "NP", Version: "1.0.0", Type: TypeNode},
	}
	handler := func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) { return nil, nil }
	WithNode("my_node", map[string]any{"fields": []string{"a"}}, handler)(s)

	conn := startTestServer(t, s)
	client := pb.NewPluginLifecycleClient(conn)

	resp, err := client.Handshake(context.Background(), &pb.HandshakeRequest{ProtocolVersion: 1})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Type != "my_node" {
		t.Errorf("nodes = %v", resp.Nodes)
	}
}

func TestLifecycleInitializeAndShutdown(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	conn := startTestServer(t, s)
	client := pb.NewPluginLifecycleClient(conn)

	_, err := client.Initialize(context.Background(), &pb.InitializeRequest{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	_, err = client.Shutdown(context.Background(), &pb.ShutdownRequest{})
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestLifecycleHealthCheck(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	conn := startTestServer(t, s)
	client := pb.NewPluginLifecycleClient(conn)

	resp, err := client.HealthCheck(context.Background(), &pb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if !resp.Healthy {
		t.Error("expected healthy = true")
	}
}

func TestToolServiceListAndExecute(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	WithTool("add", "add numbers", nil, func(ctx context.Context, args string) (string, error) {
		var input struct{ A, B int }
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d", input.A+input.B), nil
	})(s)

	conn := startTestServer(t, s)
	client := pb.NewToolServiceClient(conn)

	listResp, err := client.ListTools(context.Background(), &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listResp.Tools) != 1 || listResp.Tools[0].Name != "add" {
		t.Errorf("tools = %v", listResp.Tools)
	}

	execResp, err := client.Execute(context.Background(), &pb.ToolExecuteRequest{
		Name:      "add",
		Arguments: `{"A":3,"B":4}`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if execResp.Result != "7" {
		t.Errorf("result = %q, want %q", execResp.Result, "7")
	}
	if execResp.Error != "" {
		t.Errorf("unexpected error: %q", execResp.Error)
	}
}

func TestToolServiceExecuteNotFound(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	conn := startTestServer(t, s)
	client := pb.NewToolServiceClient(conn)

	resp, err := client.Execute(context.Background(), &pb.ToolExecuteRequest{
		Name:      "nonexistent",
		Arguments: "{}",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected error for nonexistent tool")
	}
}

func TestToolServiceExecuteHandlerError(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	WithTool("fail", "always fails", nil, func(ctx context.Context, args string) (string, error) {
		return "", fmt.Errorf("something broke")
	})(s)

	conn := startTestServer(t, s)
	client := pb.NewToolServiceClient(conn)

	resp, err := client.Execute(context.Background(), &pb.ToolExecuteRequest{
		Name:      "fail",
		Arguments: "{}",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.Error != "something broke" {
		t.Errorf("error = %q, want %q", resp.Error, "something broke")
	}
}

func TestNodeServiceListNodes(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	handler := func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) { return nil, nil }
	WithNode("nodeA", map[string]any{"x": 1}, handler)(s)
	WithNode("nodeB", nil, handler)(s)

	conn := startTestServer(t, s)
	client := pb.NewNodeServiceClient(conn)

	resp, err := client.ListNodes(context.Background(), &pb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(resp.Nodes))
	}
}

func TestNodeServiceExecuteStream(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	WithNode("echo", nil, func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) {
		return map[string]any{"message": "hello from node"}, nil
	})(s)

	conn := startTestServer(t, s)
	client := pb.NewNodeServiceClient(conn)

	stream, err := client.Execute(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	err = stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteRequest{
			ExecuteRequest: &pb.NodeExecuteRequest{NodeId: "echo"},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	execResp := resp.GetExecuteResponse()
	if execResp == nil {
		t.Fatal("expected ExecuteResponse")
	}
	if execResp.Error != "" {
		t.Fatalf("unexpected error: %s", execResp.Error)
	}

	val, ok := execResp.Outputs["message"]
	if !ok {
		t.Fatal("missing 'message' in outputs")
	}
	if val != `"hello from node"` {
		t.Errorf("message = %q, want %q", val, `"hello from node"`)
	}
}

func TestNodeServiceExecuteFallbackToFirstNode(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	WithNode("only", nil, func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})(s)

	conn := startTestServer(t, s)
	client := pb.NewNodeServiceClient(conn)

	stream, err := client.Execute(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Send with a wrong node ID — should fallback to the only registered node
	err = stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteRequest{
			ExecuteRequest: &pb.NodeExecuteRequest{NodeId: "wrong_id"},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.GetExecuteResponse().Error != "" {
		t.Errorf("unexpected error: %s", resp.GetExecuteResponse().Error)
	}
}

func TestNodeServiceExecuteNodeError(t *testing.T) {
	s := &server{tools: make(map[string]toolEntry), nodes: make(map[string]nodeEntry)}
	WithNode("bad", nil, func(ctx context.Context, cb NodeCallbacks) (map[string]any, error) {
		return nil, fmt.Errorf("node failed")
	})(s)

	conn := startTestServer(t, s)
	client := pb.NewNodeServiceClient(conn)

	stream, err := client.Execute(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteRequest{
			ExecuteRequest: &pb.NodeExecuteRequest{NodeId: "bad"},
		},
	})

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.GetExecuteResponse().Error != "node failed" {
		t.Errorf("error = %q, want %q", resp.GetExecuteResponse().Error, "node failed")
	}
}

func TestContextMethods(t *testing.T) {
	var gotKey, gotSetKey, gotSetVal string
	var gotPrompt, gotToolName, gotToolArgs, gotCmd, gotSigType string
	var gotEmit string
	var gotPayload any

	mock := &mockCallbacks{
		getVar: func(key string) (any, bool) {
			gotKey = key
			return "value-42", true
		},
		setVar: func(key string, value any) {
			gotSetKey = key
			gotSetVal = value.(string)
		},
		llmGenerate: func(ctx context.Context, prompt string) (string, error) {
			gotPrompt = prompt
			return "generated", nil
		},
		toolExecute: func(ctx context.Context, name, args string) (string, error) {
			gotToolName = name
			gotToolArgs = args
			return "tool-result", nil
		},
		streamEmit: func(data string) { gotEmit = data },
		sandboxExec: func(ctx context.Context, command string) (string, error) {
			gotCmd = command
			return "exec-output", nil
		},
		signal: func(ctx context.Context, signalType string, payload any) error {
			gotSigType = signalType
			gotPayload = payload
			return nil
		},
	}

	c := &Context{ctx: context.Background(), callbacks: mock}

	if v, ok := c.GetVar("mykey"); !ok || v != "value-42" || gotKey != "mykey" {
		t.Errorf("GetVar failed: v=%v ok=%v", v, ok)
	}

	c.SetVar("out", "hello")
	if gotSetKey != "out" || gotSetVal != "hello" {
		t.Errorf("SetVar: key=%q val=%q", gotSetKey, gotSetVal)
	}

	res, err := c.LLMGenerate("prompt1")
	if err != nil || res != "generated" || gotPrompt != "prompt1" {
		t.Errorf("LLMGenerate: res=%q err=%v", res, err)
	}

	res, err = c.ToolExecute("search", `{"q":"test"}`)
	if err != nil || res != "tool-result" || gotToolName != "search" || gotToolArgs != `{"q":"test"}` {
		t.Errorf("ToolExecute: res=%q err=%v", res, err)
	}

	c.StreamEmit("chunk1")
	if gotEmit != "chunk1" {
		t.Errorf("StreamEmit: got %q", gotEmit)
	}

	res, err = c.SandboxExec("ls -la")
	if err != nil || res != "exec-output" || gotCmd != "ls -la" {
		t.Errorf("SandboxExec: res=%q err=%v", res, err)
	}

	err = c.Signal("broadcast", map[string]string{"msg": "hi"})
	if err != nil || gotSigType != "broadcast" {
		t.Errorf("Signal: err=%v type=%q", err, gotSigType)
	}
	if gotPayload == nil {
		t.Error("Signal: payload is nil")
	}
}

// mockCallbacks implements NodeCallbacks for unit testing Context methods.
type mockCallbacks struct {
	getVar      func(key string) (any, bool)
	setVar      func(key string, value any)
	llmGenerate func(ctx context.Context, prompt string) (string, error)
	toolExecute func(ctx context.Context, name string, args string) (string, error)
	streamEmit  func(data string)
	sandboxExec func(ctx context.Context, command string) (string, error)
	signal      func(ctx context.Context, signalType string, payload any) error
}

func (m *mockCallbacks) GetVar(key string) (any, bool) { return m.getVar(key) }
func (m *mockCallbacks) SetVar(key string, value any)   { m.setVar(key, value) }
func (m *mockCallbacks) LLMGenerate(ctx context.Context, prompt string) (string, error) {
	return m.llmGenerate(ctx, prompt)
}
func (m *mockCallbacks) ToolExecute(ctx context.Context, name string, args string) (string, error) {
	return m.toolExecute(ctx, name, args)
}
func (m *mockCallbacks) StreamEmit(data string) { m.streamEmit(data) }
func (m *mockCallbacks) SandboxExec(ctx context.Context, command string) (string, error) {
	return m.sandboxExec(ctx, command)
}
func (m *mockCallbacks) Signal(ctx context.Context, signalType string, payload any) error {
	return m.signal(ctx, signalType, payload)
}
