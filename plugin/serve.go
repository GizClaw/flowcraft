// Package plugin provides a development kit for building external FlowCraft plugins.
// External plugins are standalone binaries that communicate with the host via
// gRPC over Unix domain socket.
//
// Usage:
//
//	func main() {
//	    plugin.Serve(
//	        plugin.WithInfo(plugin.PluginInfo{...}),
//	        plugin.WithTool("my_tool", "description", nil, myHandler),
//	    )
//	}
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/GizClaw/flowcraft/plugin/proto"
	"google.golang.org/grpc"
)

// ToolHandler processes a tool call from the host.
type ToolHandler func(ctx context.Context, arguments string) (string, error)

// NodeHandler processes a node execution from the host.
type NodeHandler func(ctx context.Context, callbacks NodeCallbacks) (map[string]any, error)

// Context wraps host callback functions available to plugin handlers.
type Context struct {
	ctx       context.Context
	callbacks NodeCallbacks
}

// GetVar reads a variable from the host board.
func (c *Context) GetVar(key string) (any, bool) { return c.callbacks.GetVar(key) }

// SetVar writes a variable to the host board.
func (c *Context) SetVar(key string, value any) { c.callbacks.SetVar(key, value) }

// LLMGenerate calls the host's LLM for text generation.
func (c *Context) LLMGenerate(prompt string) (string, error) {
	return c.callbacks.LLMGenerate(c.ctx, prompt)
}

// ToolExecute calls a host-side tool.
func (c *Context) ToolExecute(name, args string) (string, error) {
	return c.callbacks.ToolExecute(c.ctx, name, args)
}

// StreamEmit sends streaming data to the host.
func (c *Context) StreamEmit(data string) { c.callbacks.StreamEmit(data) }

// SandboxExec executes a command in the host sandbox.
func (c *Context) SandboxExec(command string) (string, error) {
	return c.callbacks.SandboxExec(c.ctx, command)
}

// Signal sends a signal to the host (e.g. broadcast to other agents).
func (c *Context) Signal(signalType string, payload any) error {
	return c.callbacks.Signal(c.ctx, signalType, payload)
}

// Option configures the plugin server.
type Option func(*server)

// WithInfo sets the plugin metadata.
func WithInfo(info PluginInfo) Option {
	return func(s *server) { s.info = info }
}

// WithTool registers a tool handler.
func WithTool(name string, description string, schema map[string]any, handler ToolHandler) Option {
	return func(s *server) {
		s.tools[name] = toolEntry{
			spec: ToolSpec{
				Name:        name,
				Description: description,
				InputSchema: schema,
			},
			handler: handler,
		}
	}
}

// WithNode registers a node handler.
func WithNode(nodeType string, schema map[string]any, handler NodeHandler) Option {
	return func(s *server) {
		s.nodes[nodeType] = nodeEntry{
			spec:    NodeSpec{Type: nodeType, Schema: schema},
			handler: handler,
		}
	}
}

type toolEntry struct {
	spec    ToolSpec
	handler ToolHandler
}

type nodeEntry struct {
	spec    NodeSpec
	handler NodeHandler
}

type server struct {
	info  PluginInfo
	tools map[string]toolEntry
	nodes map[string]nodeEntry
}

// lifecycleServer implements PluginLifecycleServer.
type lifecycleServer struct {
	pb.UnimplementedPluginLifecycleServer
	s *server
}

func (g *lifecycleServer) Handshake(_ context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	resp := &pb.HandshakeResponse{
		PluginInfo: &pb.PluginInfo{
			Id:          g.s.info.ID,
			Name:        g.s.info.Name,
			Version:     g.s.info.Version,
			Type:        string(g.s.info.Type),
			Description: g.s.info.Description,
			Author:      g.s.info.Author,
			Builtin:     g.s.info.Builtin,
		},
		ProtocolVersion: req.ProtocolVersion,
	}
	for _, t := range g.s.tools {
		schemaJSON, _ := json.Marshal(t.spec.InputSchema)
		resp.Tools = append(resp.Tools, &pb.ToolSpec{
			Name:            t.spec.Name,
			Description:     t.spec.Description,
			InputSchemaJson: string(schemaJSON),
		})
	}
	for _, n := range g.s.nodes {
		schemaJSON, _ := json.Marshal(n.spec.Schema)
		resp.Nodes = append(resp.Nodes, &pb.NodeSpec{
			Type:       n.spec.Type,
			SchemaJson: string(schemaJSON),
		})
	}
	return resp, nil
}

func (g *lifecycleServer) Initialize(_ context.Context, _ *pb.InitializeRequest) (*pb.InitializeResponse, error) {
	return &pb.InitializeResponse{}, nil
}

func (g *lifecycleServer) Shutdown(_ context.Context, _ *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return &pb.ShutdownResponse{}, nil
}

func (g *lifecycleServer) HealthCheck(_ context.Context, _ *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{Healthy: true}, nil
}

// toolServer implements ToolServiceServer.
type toolServer struct {
	pb.UnimplementedToolServiceServer
	s *server
}

func (g *toolServer) ListTools(_ context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	resp := &pb.ListToolsResponse{}
	for _, t := range g.s.tools {
		schemaJSON, _ := json.Marshal(t.spec.InputSchema)
		resp.Tools = append(resp.Tools, &pb.ToolSpec{
			Name:            t.spec.Name,
			Description:     t.spec.Description,
			InputSchemaJson: string(schemaJSON),
		})
	}
	return resp, nil
}

func (g *toolServer) Execute(ctx context.Context, req *pb.ToolExecuteRequest) (*pb.ToolExecuteResponse, error) {
	entry, ok := g.s.tools[req.Name]
	if !ok {
		return &pb.ToolExecuteResponse{Error: fmt.Sprintf("tool %q not found", req.Name)}, nil
	}
	result, err := entry.handler(ctx, req.Arguments)
	if err != nil {
		return &pb.ToolExecuteResponse{Error: err.Error()}, nil
	}
	return &pb.ToolExecuteResponse{Result: result}, nil
}

// nodeServer implements NodeServiceServer.
type nodeServer struct {
	pb.UnimplementedNodeServiceServer
	s *server
}

func (g *nodeServer) ListNodes(_ context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	resp := &pb.ListNodesResponse{}
	for _, n := range g.s.nodes {
		schemaJSON, _ := json.Marshal(n.spec.Schema)
		resp.Nodes = append(resp.Nodes, &pb.NodeSpec{
			Type:       n.spec.Type,
			SchemaJson: string(schemaJSON),
		})
	}
	return resp, nil
}

// Execute implements the bidirectional streaming NodeService.Execute RPC.
// Protocol: host sends ExecuteRequest → plugin calls NodeHandler (which may
// issue GetVar/SetVar/LLM/Tool/Sandbox/Signal callbacks) → plugin sends
// ExecuteResponse.
func (g *nodeServer) Execute(stream grpc.BidiStreamingServer[pb.NodeMessage, pb.NodeMessage]) error {
	initMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("plugin: recv execute request: %w", err)
	}

	execReq := initMsg.GetExecuteRequest()
	if execReq == nil {
		return fmt.Errorf("plugin: expected ExecuteRequest, got %T", initMsg.Msg)
	}

	entry, ok := g.s.nodes[execReq.NodeId]
	if !ok {
		for _, e := range g.s.nodes {
			entry = e
			ok = true
			break
		}
		if !ok {
			return sendNodeError(stream, fmt.Sprintf("node %q not registered", execReq.NodeId))
		}
	}

	cb := &streamCallbacks{stream: stream}

	result, err := entry.handler(stream.Context(), cb)
	if err != nil {
		return sendNodeError(stream, err.Error())
	}

	outputMap := make(map[string]string, len(result))
	for k, v := range result {
		b, _ := json.Marshal(v)
		outputMap[k] = string(b)
	}

	return stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteResponse{
			ExecuteResponse: &pb.NodeExecuteResponse{
				Outputs: outputMap,
			},
		},
	})
}

func sendNodeError(stream grpc.BidiStreamingServer[pb.NodeMessage, pb.NodeMessage], errMsg string) error {
	return stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteResponse{
			ExecuteResponse: &pb.NodeExecuteResponse{
				Error: errMsg,
			},
		},
	})
}

// streamCallbacks bridges NodeCallbacks over the gRPC bidirectional stream.
type streamCallbacks struct {
	stream grpc.BidiStreamingServer[pb.NodeMessage, pb.NodeMessage]
}

func (s *streamCallbacks) GetVar(key string) (any, bool) {
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_GetVarRequest{
			GetVarRequest: &pb.GetVarRequest{Key: key},
		},
	})
	resp, err := s.stream.Recv()
	if err != nil {
		return nil, false
	}
	gvr := resp.GetGetVarResponse()
	if gvr == nil {
		return nil, false
	}
	if !gvr.Found {
		return nil, false
	}
	var val any
	_ = json.Unmarshal([]byte(gvr.Value), &val)
	return val, true
}

func (s *streamCallbacks) SetVar(key string, value any) {
	b, _ := json.Marshal(value)
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_SetVarRequest{
			SetVarRequest: &pb.SetVarRequest{Key: key, Value: string(b)},
		},
	})
}

func (s *streamCallbacks) LLMGenerate(ctx context.Context, prompt string) (string, error) {
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_LlmGenerateRequest{
			LlmGenerateRequest: &pb.LLMGenerateRequest{Prompt: prompt},
		},
	})
	resp, err := s.stream.Recv()
	if err != nil {
		return "", fmt.Errorf("plugin: llm generate recv: %w", err)
	}
	llmResp := resp.GetLlmGenerateResponse()
	if llmResp == nil {
		return "", fmt.Errorf("plugin: expected LLMGenerateResponse")
	}
	if llmResp.Error != "" {
		return "", fmt.Errorf("plugin: llm: %s", llmResp.Error)
	}
	return llmResp.Result, nil
}

func (s *streamCallbacks) ToolExecute(ctx context.Context, name, args string) (string, error) {
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ToolExecuteRequest{
			ToolExecuteRequest: &pb.ToolExecuteRequest{Name: name, Arguments: args},
		},
	})
	resp, err := s.stream.Recv()
	if err != nil {
		return "", fmt.Errorf("plugin: tool execute recv: %w", err)
	}
	toolResp := resp.GetToolExecuteResponse()
	if toolResp == nil {
		return "", fmt.Errorf("plugin: expected ToolExecuteResponse")
	}
	if toolResp.Error != "" {
		return "", fmt.Errorf("plugin: tool %q: %s", name, toolResp.Error)
	}
	return toolResp.Result, nil
}

func (s *streamCallbacks) StreamEmit(data string) {
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_StreamEmitRequest{
			StreamEmitRequest: &pb.StreamEmitRequest{Data: data},
		},
	})
}

func (s *streamCallbacks) SandboxExec(ctx context.Context, command string) (string, error) {
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_SandboxExecRequest{
			SandboxExecRequest: &pb.SandboxExecRequest{Command: command},
		},
	})
	resp, err := s.stream.Recv()
	if err != nil {
		return "", fmt.Errorf("plugin: sandbox exec recv: %w", err)
	}
	sbResp := resp.GetSandboxExecResponse()
	if sbResp == nil {
		return "", fmt.Errorf("plugin: expected SandboxExecResponse")
	}
	if sbResp.Error != "" {
		return "", fmt.Errorf("plugin: sandbox: %s", sbResp.Error)
	}
	return sbResp.Output, nil
}

func (s *streamCallbacks) Signal(ctx context.Context, signalType string, payload any) error {
	payloadBytes, _ := json.Marshal(payload)
	_ = s.stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_SignalRequest{
			SignalRequest: &pb.SignalRequest{
				SignalType: signalType,
				Payload:    payloadBytes,
			},
		},
	})
	resp, err := s.stream.Recv()
	if err != nil {
		return fmt.Errorf("plugin: signal recv: %w", err)
	}
	sigResp := resp.GetSignalResponse()
	if sigResp == nil {
		return fmt.Errorf("plugin: expected SignalResponse")
	}
	if sigResp.Error != "" {
		return fmt.Errorf("plugin: signal: %s", sigResp.Error)
	}
	return nil
}

// Serve starts the plugin gRPC server on a Unix domain socket.
// The socket path is provided by the host via FLOWCRAFT_SOCKET env var.
func Serve(opts ...Option) {
	s := &server{
		tools: make(map[string]toolEntry),
		nodes: make(map[string]nodeEntry),
	}
	for _, opt := range opts {
		opt(s)
	}

	socketPath := os.Getenv("FLOWCRAFT_SOCKET")
	if socketPath == "" {
		slog.Error("plugin: FLOWCRAFT_SOCKET not set")
		os.Exit(1)
	}

	_ = os.Remove(socketPath)
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		slog.Error("plugin: listen failed", "path", socketPath, "error", err)
		os.Exit(1)
	}

	grpcSrv := grpc.NewServer()
	pb.RegisterPluginLifecycleServer(grpcSrv, &lifecycleServer{s: s})
	pb.RegisterToolServiceServer(grpcSrv, &toolServer{s: s})
	pb.RegisterNodeServiceServer(grpcSrv, &nodeServer{s: s})

	slog.Info("plugin: serving", "id", s.info.ID, "socket", socketPath, "tools", len(s.tools), "nodes", len(s.nodes))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("plugin: shutting down", "id", s.info.ID)
		grpcSrv.GracefulStop()
	}()

	if err := grpcSrv.Serve(lis); err != nil {
		slog.Error("plugin: serve failed", "error", err)
		os.Exit(1)
	}
}
