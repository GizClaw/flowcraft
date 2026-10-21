package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	scriptnode "github.com/GizClaw/flowcraft/core/graph/nodes/script"
	res "github.com/GizClaw/flowcraft/core/resource"
)

// NodeTypeKind is the deployment resource kind of a graph node type: a graph
// node type is a deployment resource. Factory values implement
// [coregraph.NodeTypeRegistrar]; the graph engine factory registers every
// "node_type" dep before Build, so node types participate in the resource DAG
// like any other collaborator — they declare their own deps (runtime,
// workspace, tools, inference, ...), can be shared across agents, and are
// built exactly once.
//
// There is no built-in type. Every name a graph definition uses in
// NodeDefinition.Type is provided by a resource of this kind, including
// "inference", "tool" and "script": the engine holds no default registry, so
// which collaborators a type runs against is a deployment decision rather than
// an engine-wide one. Two inference types with different assemblies, or a JS
// and a Lua script type in one graph, are therefore an ordinary deployment.
const NodeTypeKind = "graph.NodeType"

// Impl names of the node type resources this package ships. The default type
// name of each impl is the impl name, so the common deployment names no type
// at all.
const (
	// NodeTypeImplInference runs core/graph/nodes' "inference" node type
	// (one Generate call per invocation).
	NodeTypeImplInference = "inference"

	// NodeTypeImplTool runs core/graph/nodes' "tool" node type (execute
	// the channel tail's tool calls as one batch).
	NodeTypeImplTool = "tool"

	// NodeTypeImplScript runs the generic "script" node type: the handler
	// source comes from each node's own config, so one type serves every
	// script of the graph.
	NodeTypeImplScript = "script"

	// NodeTypeImplScriptInline runs a script-backed type whose handler
	// source is fixed in the resource's settings, so each graph node of
	// the type is one invocation of the same script with its own config.
	NodeTypeImplScriptInline = "script_inline"
)

// builtNodeType is the value every node type impl hands the engine: one type
// name, its behaviour, and the config fields of that type that may carry a
// structured source reference.
//
// It carries the name because a resource — not the kernel — decides what a
// graph may call a type; the kernel only sees a registry.
type builtNodeType[C any] struct {
	typeName      string
	fileRefFields []string
	node          coregraph.NodeType[C]
}

// Register implements coregraph.NodeTypeRegistrar.
func (b *builtNodeType[C]) Register(reg *coregraph.Registry) error {
	return coregraph.RegisterType(reg, b.typeName, b.node)
}

// NodeTypeNames implements coregraph.TypeNameProvider.
func (b *builtNodeType[C]) NodeTypeNames() []string { return []string{b.typeName} }

// FileRefFields implements coregraph.ConfigFileRefFields.
func (b *builtNodeType[C]) FileRefFields() map[string][]string {
	if len(b.fileRefFields) == 0 {
		return nil
	}
	return map[string][]string{b.typeName: b.fileRefFields}
}

// NodeTypeSettings is the settings every standard node type impl shares.
type NodeTypeSettings struct {
	// Type is the name graph definitions use in NodeDefinition.Type. It
	// defaults to the impl name, so a deployment that wants one inference
	// type names nothing and a deployment that wants two sets it on one of
	// them.
	Type string `json:"type,omitempty"`
}

// typeName resolves the registered name: the configured one, else the impl's.
func (s NodeTypeSettings) typeName(impl string) (string, error) {
	if s.Type == "" {
		return impl, nil
	}
	if strings.ContainsAny(s.Type, " \t\n") || strings.TrimSpace(s.Type) != s.Type {
		return "", errdefs.Validationf(
			"%s: settings.type %q must not contain whitespace", impl, s.Type)
	}
	return s.Type, nil
}

// ScriptInlineSettings is the strict settings subtree of the
// script-backed impl whose handler source lives in the resource.
type ScriptInlineSettings struct {
	// Type is the node type name graphs reference in NodeDefinition.Type
	// (e.g. "extract"). It must not collide with another mounted type.
	Type string `json:"type,omitempty"`

	// Runtime names the wired agent.ScriptRuntime this type executes on.
	// Optional: with exactly one runtime dep it is selected implicitly,
	// and with several it is required.
	Runtime string `json:"runtime,omitempty"`

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

// ScriptInlineNodeTypeFactory builds a script-backed node type whose handler
// source is fixed in settings: each graph node of the type runs that source on
// the wired script runtime with the standard script bridges (board, expr, host,
// run, tools, inference, node, stream, parallel, fs, shell, runtime).
//
// The node's config is the script's "config" global, so config values may carry
// ${board:*} references like any other node config. Use impl "script" instead
// when the source itself belongs to each node.
type ScriptInlineNodeTypeFactory struct{}

// Spec implements res.Factory.
func (ScriptInlineNodeTypeFactory) Spec() res.Spec {
	return res.Spec{
		Kind: NodeTypeKind,
		Impl: NodeTypeImplScriptInline,
		Deps: []res.Requirement{
			depScriptRuntimes.Spec(),
			depTools.Spec(),
			depInference.Spec(),
			depRouter.Spec(),
			depWorkspace.Spec(),
			depSandbox.Spec(),
		},
	}
}

// New implements res.Factory.
func (ScriptInlineNodeTypeFactory) New(
	ctx context.Context,
	in res.Input,
) (*builtNodeType[map[string]any], error) {
	settings, err := res.DecodeTyped[ScriptInlineSettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph node type: decode settings: %w", err)
	}
	typeName, err := NodeTypeSettings{Type: settings.Type}.typeName(NodeTypeImplScriptInline)
	if err != nil {
		return nil, err
	}
	if len(settings.Source) == 0 {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source is required", typeName)
	}
	src, err := res.ParseSource(settings.Source)
	if err != nil {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source: %w", typeName, err)
	}
	data, err := resolveSource(ctx, in, src)
	if err != nil {
		return nil, fmt.Errorf("graph node type %q: settings.source: %w", typeName, err)
	}
	if len(data) == 0 {
		return nil, errdefs.Validationf(
			"graph node type %q: settings.source is empty", typeName)
	}

	runtimes, err := decodeScriptRuntimes(in)
	if err != nil {
		return nil, err
	}
	runtime, err := runtimes.pick(settings.Runtime)
	if err != nil {
		return nil, errdefs.Validationf("graph node type %q: %w", typeName, err)
	}
	caps, err := decodeCapabilities(in)
	if err != nil {
		return nil, err
	}

	// The node type carries its own deps as the fallback surface; an
	// engine-level "script_bindings" provider replaces it (see
	// scriptnode.RunScript).
	scriptDeps := scriptNodeDeps(caps, nil)

	desc := settings.Desc
	if desc == "" {
		desc = "custom script-backed node type"
	}
	source := string(data)
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
	return &builtNodeType[map[string]any]{typeName: typeName, node: node}, nil
}
