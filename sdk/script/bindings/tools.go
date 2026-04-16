package bindings

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/rs/xid"
)

type toolBridgeConfig struct {
	reg      *tool.Registry
	allowed  map[string]bool
	allowAll bool
}

// ToolBridgeOption configures NewToolBridge.
type ToolBridgeOption func(*toolBridgeConfig)

// WithToolAllowAll allows calling any tool registered in the registry.
// Use only when scripts are fully trusted.
func WithToolAllowAll() ToolBridgeOption {
	return func(c *toolBridgeConfig) { c.allowAll = true }
}

// WithAllowedToolNames restricts script-visible tools; names must match registry entries.
func WithAllowedToolNames(names ...string) ToolBridgeOption {
	return func(c *toolBridgeConfig) {
		if c.allowed == nil {
			c.allowed = make(map[string]bool)
		}
		for _, n := range names {
			c.allowed[n] = true
		}
	}
}

// NewToolBridge exposes tool execution to scripts as global "tools":
//   - call(name, argumentsJSON) -> { content, is_error, tool_call_id }
//   - list() -> []string (names the script is allowed to call)
//
// Security: by default no tool is callable until WithAllowedToolNames or WithToolAllowAll is set.
func NewToolBridge(reg *tool.Registry, opts ...ToolBridgeOption) BindingFunc {
	cfg := &toolBridgeConfig{reg: reg}
	for _, o := range opts {
		o(cfg)
	}
	return func(ctx context.Context) (string, any) {
		return "tools", map[string]any{
			"call": func(name string, argumentsJSON string) (map[string]any, error) {
				if cfg.reg == nil {
					return map[string]any{
						"content":      "tools: no registry configured",
						"is_error":     true,
						"tool_call_id": "",
					}, nil
				}
				if !cfg.allowAll {
					if cfg.allowed == nil || !cfg.allowed[name] {
						return map[string]any{
							"content":      fmt.Sprintf("tools: tool %q is not allowed for this script", name),
							"is_error":     true,
							"tool_call_id": "",
						}, nil
					}
				} else if _, ok := cfg.reg.Get(name); !ok {
					return map[string]any{
						"content":      fmt.Sprintf("tools: unknown tool %q", name),
						"is_error":     true,
						"tool_call_id": "",
					}, nil
				}
				call := model.ToolCall{
					ID:        xid.New().String(),
					Name:      name,
					Arguments: argumentsJSON,
				}
				res := cfg.reg.Execute(ctx, call)
				return map[string]any{
					"content":      res.Content,
					"is_error":     res.IsError,
					"tool_call_id": res.ToolCallID,
				}, nil
			},
			"list": func() []string {
				if cfg.reg == nil {
					return nil
				}
				names := cfg.reg.Names()
				if cfg.allowAll {
					return append([]string(nil), names...)
				}
				if len(cfg.allowed) == 0 {
					return nil
				}
				out := make([]string, 0, len(names))
				for _, n := range names {
					if cfg.allowed[n] {
						out = append(out, n)
					}
				}
				return out
			},
		}
	}
}
