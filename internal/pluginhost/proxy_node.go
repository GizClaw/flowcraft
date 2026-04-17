package pluginhost

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// HostCallbackProvider supplies host-side callback implementations.
// Injected at construction time to avoid circular imports with tool/.
type HostCallbackProvider struct {
	LLMGenerate func(ctx context.Context, prompt string) (string, error)
	ToolExecute func(ctx context.Context, name string, args string) (string, error)
	SandboxExec func(ctx context.Context, command string) (string, error)
	Signal      func(ctx context.Context, signalType string, payload any) error
}

// NodeClientResolver dynamically resolves the latest gRPC NodeServiceClient
// for a given pluginID. This indirection enables hot-reloading: when a plugin
// is restarted, pre-compiled graphs automatically pick up the new connection.
type NodeClientResolver func(pluginID string) (plugin.NodeServiceClient, error)

// ProxyNodeOption configures optional ProxyNode fields.
type ProxyNodeOption func(*ProxyNode)

// WithPorts overrides the default any-typed input/output ports.
func WithPorts(in, out []graph.Port) ProxyNodeOption {
	return func(p *ProxyNode) {
		if len(in) > 0 {
			p.inputPorts = in
		}
		if len(out) > 0 {
			p.outputPorts = out
		}
	}
}

// ProxyNode wraps an external NodePlugin's node as a local graph-executable type.
// It delegates execution to the external plugin, forwarding host callbacks
// (GetVar/SetVar/LLMGenerate/ToolExecute/StreamEmit/SandboxExec) via the
// NodeCallbacks interface.
type ProxyNode struct {
	id             string
	nodeType       string
	config         map[string]any
	pluginID       string
	clientResolver NodeClientResolver
	host           *HostCallbackProvider
	inputPorts     []graph.Port
	outputPorts    []graph.Port
}

// NewProxyNode creates a proxy for an external node.
// host may be nil, in which case LLM/Tool/Sandbox callbacks return errors.
func NewProxyNode(id, nodeType, pluginID string, config map[string]any, resolver NodeClientResolver, host *HostCallbackProvider, opts ...ProxyNodeOption) *ProxyNode {
	p := &ProxyNode{
		id:             id,
		nodeType:       nodeType,
		config:         config,
		pluginID:       pluginID,
		clientResolver: resolver,
		host:           host,
		inputPorts:     []graph.Port{{Name: "input", Type: graph.PortTypeAny}},
		outputPorts:    []graph.Port{{Name: "output", Type: graph.PortTypeAny}},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ID returns the node identifier.
func (p *ProxyNode) ID() string { return p.id }

// Type returns the node type name.
func (p *ProxyNode) Type() string { return p.nodeType }

// InputPorts returns the declared input ports.
func (p *ProxyNode) InputPorts() []graph.Port { return p.inputPorts }

// OutputPorts returns the declared output ports.
func (p *ProxyNode) OutputPorts() []graph.Port { return p.outputPorts }

// ExecuteBoard runs the external node, bridging host callbacks to the plugin.
func (p *ProxyNode) ExecuteBoard(execCtx graph.ExecutionContext, board *graph.Board) error {
	ctx := execCtx.Context
	if p.clientResolver == nil {
		return fmt.Errorf("proxy_node %q: no client resolver configured", p.id)
	}

	client, err := p.clientResolver(p.pluginID)
	if err != nil {
		return fmt.Errorf("proxy_node %q: resolve client: %w", p.id, err)
	}

	callbacks := &boardCallbacks{
		board:  board,
		host:   p.host,
		stream: execCtx.Stream,
		nodeID: p.id,
	}
	result, err := client.Execute(ctx, p.nodeType, p.config, callbacks)
	if err != nil {
		return fmt.Errorf("proxy_node %q: %w", p.id, err)
	}

	for k, v := range result {
		board.SetVar(k, v)
	}
	return nil
}

// boardCallbacks adapts a graph.Board + HostCallbackProvider to NodeCallbacks.
type boardCallbacks struct {
	board  *graph.Board
	host   *HostCallbackProvider
	stream graph.StreamCallback
	nodeID string
}

func (b *boardCallbacks) GetVar(key string) (any, bool) { return b.board.GetVar(key) }
func (b *boardCallbacks) SetVar(key string, value any)  { b.board.SetVar(key, value) }

func (b *boardCallbacks) LLMGenerate(ctx context.Context, prompt string) (string, error) {
	if b.host != nil && b.host.LLMGenerate != nil {
		return b.host.LLMGenerate(ctx, prompt)
	}
	return "", fmt.Errorf("proxy_node: LLMGenerate callback not configured")
}

func (b *boardCallbacks) ToolExecute(ctx context.Context, name, args string) (string, error) {
	if b.host != nil && b.host.ToolExecute != nil {
		return b.host.ToolExecute(ctx, name, args)
	}
	return "", fmt.Errorf("proxy_node: ToolExecute callback not configured")
}

func (b *boardCallbacks) StreamEmit(data string) {
	if b.stream != nil {
		b.stream(graph.StreamEvent{
			Type:    "plugin_stream",
			NodeID:  b.nodeID,
			Payload: data,
		})
	}
}

func (b *boardCallbacks) SandboxExec(ctx context.Context, command string) (string, error) {
	if b.host != nil && b.host.SandboxExec != nil {
		return b.host.SandboxExec(ctx, command)
	}
	return "", fmt.Errorf("proxy_node: SandboxExec callback not configured")
}

func (b *boardCallbacks) Signal(ctx context.Context, signalType string, payload any) error {
	if b.host != nil && b.host.Signal != nil {
		return b.host.Signal(ctx, signalType, payload)
	}
	return fmt.Errorf("proxy_node: Signal callback not configured")
}
