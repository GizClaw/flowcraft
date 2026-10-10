package craft

import (
	"context"
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/resource"
)

const (
	pluginNodeTypeKind = "graph.NodeType"
	pluginNodeTypeImpl = "mcp"
)

// pluginNodeSettings is the strict settings of one synthesized node
// type.
type pluginNodeSettings struct {
	Type    string `json:"type"`
	Plugin  string `json:"plugin"`
	Tool    string `json:"tool"`
	Timeout string `json:"timeout,omitempty"`
}

// pluginNodeFactory builds graph node types whose handler calls a
// plugin-provided MCP tool (the plugin may be written in any language,
// Python included).
type pluginNodeFactory struct {
	host PluginHost
}

func (pluginNodeFactory) Spec() resource.Spec {
	return resource.Spec{Kind: pluginNodeTypeKind, Impl: pluginNodeTypeImpl}
}

func (f pluginNodeFactory) New(
	ctx context.Context,
	in resource.Input,
) (any, error) {
	settings, err := resource.DecodeTyped[pluginNodeSettings](ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	if settings.Type == "" || settings.Plugin == "" || settings.Tool == "" {
		return nil, errdefs.Validationf(
			"craft: graph node type requires type, plugin and tool")
	}
	timeout := 30 * time.Second
	if settings.Timeout != "" {
		timeout, err = time.ParseDuration(settings.Timeout)
		if err != nil || timeout <= 0 {
			return nil, errdefs.Validationf(
				"craft: graph node %q timeout: %v", settings.Type, settings.Timeout)
		}
	}
	host := f.host
	node := coregraph.NodeType[map[string]any]{
		Meta: coregraph.Meta{Desc: "plugin-provided graph node"},
		Handler: func(
			ec coregraph.ExecutionContext,
			board *agent.Board,
			config map[string]any,
		) error {
			callCtx, cancel := context.WithTimeout(ec.Context, timeout)
			defer cancel()
			payload := map[string]any{
				"node": map[string]any{
					"id":    ec.NodeID,
					"type":  ec.NodeType,
					"graph": ec.GraphID,
				},
				"config": config,
			}
			out, err := host.CallTool(callCtx, settings.Plugin, settings.Tool, payload)
			if err != nil {
				return err
			}
			var result struct {
				Writes map[string]any `json:"writes"`
			}
			if len(out) > 0 {
				if err := json.Unmarshal(out, &result); err != nil {
					return errdefs.Validationf(
						"craft: graph node %q: decode plugin result: %v",
						settings.Type, err)
				}
			}
			for key, value := range result.Writes {
				board.SetVar(key, value)
			}
			return nil
		},
	}
	return pluginNodeRegistrar{name: settings.Type, node: node}, nil
}

type pluginNodeRegistrar struct {
	name string
	node coregraph.NodeType[map[string]any]
}

func (r pluginNodeRegistrar) Register(registry *coregraph.Registry) error {
	return coregraph.RegisterType(registry, r.name, r.node)
}
