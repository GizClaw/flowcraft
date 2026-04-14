package plugin

import "context"

// PluginProtocol defines the interface for external plugins.
// Communication uses gRPC over Unix domain socket.
// See plugin/proto/plugin.proto for the wire protocol definition.

// LifecycleClient is the client-side interface for plugin lifecycle management.
type LifecycleClient interface {
	Handshake(ctx context.Context, req *HandshakeRequest) (*HandshakeResponse, error)
	Initialize(ctx context.Context, config map[string]any) error
	Shutdown(ctx context.Context) error
	HealthCheck(ctx context.Context) error
}

// ToolServiceClient is the client-side interface for external tool execution.
type ToolServiceClient interface {
	ListTools(ctx context.Context) ([]ToolSpec, error)
	Execute(ctx context.Context, name string, arguments string) (string, error)
}

// NodeServiceClient is the client-side interface for external node execution.
type NodeServiceClient interface {
	ListNodes(ctx context.Context) ([]NodeSpec, error)
	Execute(ctx context.Context, nodeID string, config map[string]any, callbacks NodeCallbacks) (map[string]any, error)
}

// HandshakeRequest is sent by host to plugin after connection.
type HandshakeRequest struct {
	HostVersion string `json:"host_version"`
	ProtocolVer int    `json:"protocol_version"`
}

// HandshakeResponse is returned by the plugin during handshake.
type HandshakeResponse struct {
	PluginInfo  PluginInfo `json:"plugin_info"`
	ProtocolVer int        `json:"protocol_version"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	Nodes       []NodeSpec `json:"nodes,omitempty"`
}

// NodeSpec describes a node type provided by an external plugin.
type NodeSpec struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema,omitempty"`
}

// NodeCallbacks are host callbacks available to external nodes during execution.
type NodeCallbacks interface {
	GetVar(key string) (any, bool)
	SetVar(key string, value any)
	LLMGenerate(ctx context.Context, prompt string) (string, error)
	ToolExecute(ctx context.Context, name string, args string) (string, error)
	StreamEmit(data string)
	SandboxExec(ctx context.Context, command string) (string, error)
	Signal(ctx context.Context, signalType string, payload any) error
}
