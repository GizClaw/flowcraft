package pluginhost

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// ProxyTool wraps an external ToolPlugin's tool as a local tool.Tool-compatible type.
// It delegates Execute calls to the external plugin via gRPC.
type ProxyTool struct {
	spec     plugin.ToolSpec
	pluginID string
	executor func(ctx context.Context, name string, args string) (string, error)
}

// NewProxyTool creates a proxy for an external tool.
func NewProxyTool(spec plugin.ToolSpec, pluginID string, executor func(ctx context.Context, name, args string) (string, error)) *ProxyTool {
	return &ProxyTool{
		spec:     spec,
		pluginID: pluginID,
		executor: executor,
	}
}

// Definition returns the LLM tool definition.
func (p *ProxyTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        p.spec.Name,
		Description: p.spec.Description,
		InputSchema: p.spec.InputSchema,
	}
}

// Execute delegates to the external plugin.
func (p *ProxyTool) Execute(ctx context.Context, arguments string) (string, error) {
	if p.executor == nil {
		return "", fmt.Errorf("proxy_tool: no executor for %q", p.spec.Name)
	}
	return p.executor(ctx, p.spec.Name, arguments)
}
