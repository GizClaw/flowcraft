package bindings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/tool"

	"github.com/rs/xid"
)

type toolBridgeConfig struct {
	allowed  map[string]bool
	allowAll bool
}

// ToolBridgeOption configures NewToolBridge.
type ToolBridgeOption func(*toolBridgeConfig)

// WithToolAllowAll allows calling any tool in the catalog.
// Use only when scripts are fully trusted.
func WithToolAllowAll() ToolBridgeOption {
	return func(c *toolBridgeConfig) { c.allowAll = true }
}

// WithAllowedToolNames restricts script-visible tools; names must match catalog entries.
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
// dispatcher executes the calls (typically a *tool.Executor assembled
// with the middleware the host wants — approval, timeouts, audit);
// catalog answers name lookups for allow-listing and list(). Splitting
// the two keeps the bridge aligned with the tool package's
// catalog/execution separation.
//
// Security: by default no tool is callable until WithAllowedToolNames or
// WithToolAllowAll is set.
func NewToolBridge(dispatcher tool.Dispatcher, catalog tool.Catalog, opts ...ToolBridgeOption) BindingFunc {
	cfg := &toolBridgeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return func(ctx context.Context) (string, any) {
		return "tools", map[string]any{
			"call": func(name string, argumentsJSON string) (map[string]any, error) {
				deny := func(msg string) map[string]any {
					return map[string]any{"content": msg, "is_error": true, "tool_call_id": ""}
				}
				if dispatcher == nil || catalog == nil {
					return deny("tools: no dispatcher/catalog configured"), nil
				}
				if !cfg.allowAll {
					if cfg.allowed == nil || !cfg.allowed[name] {
						return deny(fmt.Sprintf("tools: tool %q is not allowed for this script", name)), nil
					}
				} else if _, ok := catalog.Get(name); !ok {
					return deny(fmt.Sprintf("tools: unknown tool %q", name)), nil
				}
				call := tool.Call{
					ID:        xid.New().String(),
					Name:      name,
					Arguments: json.RawMessage(argumentsJSON),
				}
				res := dispatcher.Execute(ctx, call)
				return map[string]any{
					"content":      res.Content,
					"is_error":     res.IsError,
					"tool_call_id": res.CallID,
				}, nil
			},
			"list": func() []string {
				if catalog == nil {
					return nil
				}
				defs := catalog.Definitions()
				out := make([]string, 0, len(defs))
				for _, d := range defs {
					if cfg.allowAll || cfg.allowed[d.Name] {
						out = append(out, d.Name)
					}
				}
				return out
			},
		}
	}
}
