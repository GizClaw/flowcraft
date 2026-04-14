// Package plugin provides an extensible plugin system supporting both built-in
// (compiled-in, init() self-registration) and external (gRPC subprocess) plugins.
package plugin

import (
	"context"
	"time"
)

// PluginType identifies the kind of plugin.
type PluginType string

const (
	TypeModel    PluginType = "model"
	TypeTool     PluginType = "tool"
	TypeNode     PluginType = "node"
	TypeStrategy PluginType = "agent_strategy"
	TypeData     PluginType = "data_source"
)

// PluginInfo contains metadata about a plugin.
type PluginInfo struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Type        PluginType `json:"type"`
	Description string     `json:"description,omitempty"`
	Author      string     `json:"author,omitempty"`
	Icon        string     `json:"icon,omitempty"`
	Homepage    string     `json:"homepage,omitempty"`
	Builtin     bool       `json:"builtin"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Plugin is the base interface for all plugins.
type Plugin interface {
	Info() PluginInfo
	Initialize(ctx context.Context, config map[string]any) error
	Shutdown(ctx context.Context) error
}

// ModelPlugin extends Plugin for LLM model providers.
type ModelPlugin interface {
	Plugin
	ModelID() string
}

// ToolPlugin extends Plugin with tool definitions and execution.
type ToolPlugin interface {
	Plugin
	Tools() []ToolSpec
	ExecuteTool(ctx context.Context, name string, arguments string) (string, error)
}

// ToolSpec describes a tool provided by a ToolPlugin.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// NodePlugin extends Plugin with a custom node type.
type NodePlugin interface {
	Plugin
	NodeType() string
	CreateNode(id string, config map[string]any) (any, error)
}

// StrategyPlugin extends Plugin with custom agent reasoning strategy.
type StrategyPlugin interface {
	Plugin
	StrategyName() string
}

// DataSourcePlugin extends Plugin for external data connectors.
type DataSourcePlugin interface {
	Plugin
	DataSourceType() string
}

// SchemaProvider is an optional interface for plugins that expose UI schemas.
type SchemaProvider interface {
	NodeSchema() map[string]any
}

// PluginStatus represents the runtime state of a plugin.
type PluginStatus string

const (
	StatusInstalled PluginStatus = "installed"
	StatusActive    PluginStatus = "active"
	StatusInactive  PluginStatus = "inactive"
	StatusError     PluginStatus = "error"
)

// InstalledPlugin wraps a plugin with its runtime status.
type InstalledPlugin struct {
	Info   PluginInfo     `json:"info"`
	Status PluginStatus   `json:"status"`
	Config map[string]any `json:"config,omitempty"`
	Error  string         `json:"error,omitempty"`
}
