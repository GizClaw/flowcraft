// Custom graph node types as deployment resources.
package resource

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	scriptnode "github.com/GizClaw/flowcraft/core/graph/nodes/script"
	res "github.com/GizClaw/flowcraft/core/resource"
)

// NodeTypeKind is the deployment resource kind of a custom graph node
// type. Factory values implement [coregraph.NodeTypeRegistrar]; the
// graph engine factory registers every "node_type" dep before Build,
// so custom node types participate in the resource DAG like any other
// collaborator — they can declare their own deps (runtime, workspace,
// tools, ...), be shared across agents, and are built exactly once.
const NodeTypeKind = "graph.NodeType"

// ScriptNodeTypeSettings is the strict settings subtree of the
// script-backed impl.
type ScriptNodeTypeSettings struct {
	// Type is the node type name graphs reference in
	// NodeDefinition.Type (e.g. "extract"). It must not collide with
	// the built-in "inference", "tool", or "script" types or with
	// another mounted custom type.
	Type string `json:"type"`

	// Source is the handler source: an inline string, or a
	// structured {"file": ...} / {"embed": ...} reference resolved
	// through the deployment loader.
	Source json.RawMessage `json:"source"`

	// Desc is the optional human-readable node type description.
	Desc string `json:"desc,omitempty"`

	// Reads and Writes declare the node type's static I/O roles. They
	// are resolved at Build time and enforced at invocation exactly
	// like the built-in types' roles; a ConfigKey-bound role reads
	// the key from the node's own config.
	Reads  []coregraph.Role `json:"reads,omitempty"`
	Writes []coregraph.Role `json:"writes,omitempty"`
}

// ScriptNodeTypeFactory builds a script-backed custom node type: each
// graph node of the type runs the declared source on the wired
// script runtime with the standard script bridges (board, expr, host,
// run, tools, inference, node, stream, parallel, fs, shell, runtime).
// The node's config is the script's "config" global, so config values
// may carry ${board.*} references like any other node config.
type ScriptNodeTypeFactory struct{}

// Spec implements res.Factory.
func (ScriptNodeTypeFactory) Spec() res.Spec {
	return res.Spec{
		Kind: NodeTypeKind,
		Impl: "script",
		Deps: []res.DepSpec{
			{Name: DepScriptRuntime, Type: "agent.ScriptRuntime", Required: true},
			{Name: DepTools, Type: "tool.Assembly"},
			{Name: DepInference, Type: "inference.Assembly"},
			{Name: DepRouter, Type: "inference.Router"},
			{Name: DepWorkspace, Type: "workspace.Workspace"},
			{Name: DepSandbox, Type: "sandbox.Runner"},
		},
	}
}

// New implements res.Factory.
func (ScriptNodeTypeFactory) New(ctx context.Context, in res.Input) (any, error) {
	settings, err := res.DecodeTyped[ScriptNodeTypeSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph node type: decode settings: %v", err)
	}
	if settings.Type == "" {
		return nil, errdefs.Validationf("graph node type: settings.type is required")
	}
	if len(settings.Source) == 0 {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source is required", settings.Type)
	}
	src, err := res.ParseSource(settings.Source)
	if err != nil {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source: %v", settings.Type, err)
	}
	data, err := resolveSource(ctx, in, src)
	if err != nil {
		return nil, fmt.Errorf("graph node type %q: settings.source: %w", settings.Type, err)
	}
	if len(data) == 0 {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source is empty", settings.Type)
	}

	deps, err := decodeDependencies(in.Deps)
	if err != nil {
		return nil, err
	}
	if deps.script == nil {
		return nil, errdefs.NotFoundf(
			"graph node type %q: requires dep %q", settings.Type, DepScriptRuntime)
	}

	scriptDeps := scriptnode.ScriptNodeDeps{
		Workspace:     deps.workspace,
		CommandRunner: deps.sandbox,
	}
	if deps.inference != nil {
		scriptDeps.InferenceAssembly = deps.inference
	}
	if deps.router != nil {
		scriptDeps.InferenceRouter = deps.router
	}
	if deps.tools != nil {
		scriptDeps.ToolDispatcher = deps.tools
		scriptDeps.ToolCatalog = deps.tools.Catalog()
	}

	desc := settings.Desc
	if desc == "" {
		desc = "custom script-backed node type"
	}
	source := string(data)
	runtime := deps.script
	node := coregraph.NodeType[map[string]any]{
		Meta: coregraph.Meta{
			Desc:   desc,
			Reads:  settings.Reads,
			Writes: settings.Writes,
		},
		Handler: func(ec coregraph.ExecutionContext, board *agent.Board, cfg map[string]any) error {
			return scriptnode.RunScript(ec, board, scriptDeps, runtime, ec.NodeID, source, cfg)
		},
	}
	return &scriptNodeType{typeName: settings.Type, node: node}, nil
}

// scriptNodeType is the resource-built value the graph engine factory
// registers into its registry.
type scriptNodeType struct {
	typeName string
	node     coregraph.NodeType[map[string]any]
}

// Register implements coregraph.NodeTypeRegistrar.
func (s *scriptNodeType) Register(reg *coregraph.Registry) error {
	return coregraph.RegisterType(reg, s.typeName, s.node)
}
