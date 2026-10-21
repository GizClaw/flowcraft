package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/graph/nodes"
	scriptnode "github.com/GizClaw/flowcraft/core/graph/nodes/script"
	res "github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The three impls below are the standard node types — "inference", "tool" and
// "script" — supplied the same way a host-provided one is: as deployment
// resources. Nothing is registered by default, so a graph never runs against
// collaborators it did not declare, and a deployment that wants two of a kind
// mounts two resources with different deps (and different settings.type).
//
// A type's static build-time contract lives with it, not with the kernel: the
// engine has no view of a node's config, so "this inference node names a tool
// the catalog does not have" is reported by the type's [coregraph.NodeType]
// ValidateConfig (see InferenceNodeTypeFactory.New).

// InferenceNodeTypeFactory builds the "inference" node type: one Generate call
// per invocation, channel tail in, one assistant message appended.
//
// Deps: "inference" (the assembly that serves an explicit config.model) and/or
// "router" (that serves a config without one; at least one of the two is
// required), plus optional "tools" for node configs that name catalog tools.
type InferenceNodeTypeFactory struct{}

// Spec implements res.Factory.
func (InferenceNodeTypeFactory) Spec() res.Spec {
	return res.Spec{
		Kind: NodeTypeKind,
		Impl: NodeTypeImplInference,
		Deps: []res.Requirement{
			depInference.Spec(),
			depRouter.Spec(),
			depTools.Spec(),
		},
	}
}

// New implements res.Factory.
func (InferenceNodeTypeFactory) New(
	ctx context.Context,
	in res.Input,
) (*builtNodeType[nodes.InferenceConfig], error) {
	settings, err := res.DecodeTyped[NodeTypeSettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph node type inference: decode settings: %w", err)
	}
	typeName, err := settings.typeName(NodeTypeImplInference)
	if err != nil {
		return nil, err
	}

	assembly, _, err := depInference.TryGet(in)
	if err != nil {
		return nil, err
	}
	router, _, err := depRouter.TryGet(in)
	if err != nil {
		return nil, err
	}
	tools, _, err := depTools.TryGet(in)
	if err != nil {
		return nil, err
	}
	if assembly == nil && router == nil {
		return nil, errdefs.NotFoundf(
			"graph node type %q: requires dep %q or %q", typeName, DepInference, DepRouter)
	}

	nodeDeps := nodes.InferenceNodeDeps{Assembly: assembly, Router: router}
	if assembly != nil {
		nodeDeps.Extensions = assembly.ExtensionDecoders()
	} else if router != nil {
		// A router-only deployment still needs the provider-carried
		// decoders of its target assembly.
		nodeDeps.Extensions = router.Target().ExtensionDecoders()
	}
	if tools != nil {
		nodeDeps.Catalog = tools.Catalog()
	}

	node := nodes.Inference(nodeDeps)
	node.ValidateConfig = inferenceConfigCheck(typeName, router != nil, tools)
	return &builtNodeType[nodes.InferenceConfig]{
		typeName:      typeName,
		fileRefFields: []string{"system_prompt"},
		node:          node,
	}, nil
}

// inferenceConfigCheck validates one inference node against the collaborators
// the type was wired with. It replaces the engine-side scan of the definition:
// the same four rules, but reported against the node that breaks them and only
// for the inference types this resource provides.
func inferenceConfigCheck(
	typeName string,
	hasRouter bool,
	tools *tool.Assembly,
) func(nodeID string, raw json.RawMessage) error {
	return func(nodeID string, raw json.RawMessage) error {
		modelConfigured, staticTools, toolsConfigured, err := scanInferenceConfig(raw)
		if err != nil {
			return errdefs.Validationf("graph node %q (type %q): %w", nodeID, typeName, err)
		}
		if !modelConfigured && !hasRouter {
			return errdefs.NotFoundf(
				"graph node %q (type %q): an inference node without config.model requires dep %q",
				nodeID, typeName, DepRouter)
		}
		if !toolsConfigured {
			return nil
		}
		if tools == nil {
			return errdefs.NotFoundf(
				"graph node %q (type %q): tools require dep %q", nodeID, typeName, DepTools)
		}
		return validateToolNames(nodeID, typeName, staticTools, tools)
	}
}

// ToolNodeTypeFactory builds the "tool" node type: it reads the tool_call parts
// off the channel tail's assistant message, executes the whole batch through the
// dispatcher (model-issued call ids preserved, so the provider can pair results
// on the next turn), and appends the results as one role=tool message.
//
// Deps: "tools" (required). Allow-listing and policy live in the dispatcher's
// middleware chain, not in the type.
type ToolNodeTypeFactory struct{}

// Spec implements res.Factory.
func (ToolNodeTypeFactory) Spec() res.Spec {
	return res.Spec{
		Kind: NodeTypeKind,
		Impl: NodeTypeImplTool,
		Deps: []res.Requirement{depToolsRequired.Spec()},
	}
}

// New implements res.Factory.
func (ToolNodeTypeFactory) New(
	ctx context.Context,
	in res.Input,
) (*builtNodeType[nodes.ToolConfig], error) {
	settings, err := res.DecodeTyped[NodeTypeSettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph node type tool: decode settings: %w", err)
	}
	typeName, err := settings.typeName(NodeTypeImplTool)
	if err != nil {
		return nil, err
	}
	tools, err := depToolsRequired.Get(in)
	if err != nil {
		return nil, err
	}
	return &builtNodeType[nodes.ToolConfig]{typeName: typeName, node: nodes.Tool(tools)}, nil
}

// ScriptNodeTypeFactory builds the generic "script" node type: each node of the
// type carries its own source and runtime in its config, and runs it on the
// wired script runtime with the standard script bridges.
//
// Deps: "script_runtime.<name>" (at least one; the suffix is the name a node's
// config.runtime selects, so one type can serve a JS node and a Lua node), plus
// optional "tools", "inference", "router", "workspace" and "sandbox" enabling
// the same globals a mounted "agent.ScriptBindings" provider would leave to it.
type ScriptNodeTypeFactory struct{}

// Spec implements res.Factory.
func (ScriptNodeTypeFactory) Spec() res.Spec {
	return res.Spec{
		Kind: NodeTypeKind,
		Impl: NodeTypeImplScript,
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
func (ScriptNodeTypeFactory) New(
	ctx context.Context,
	in res.Input,
) (*builtNodeType[scriptnode.ScriptConfig], error) {
	settings, err := res.DecodeTyped[NodeTypeSettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph node type script: decode settings: %w", err)
	}
	typeName, err := settings.typeName(NodeTypeImplScript)
	if err != nil {
		return nil, err
	}
	runtimes, err := decodeScriptRuntimes(in)
	if err != nil {
		return nil, err
	}
	if len(runtimes.named) == 0 {
		return nil, errdefs.Validationf(
			"graph node type %q: dep %q must be mounted as %q, since a node's config.runtime selects one by name",
			typeName, DepScriptRuntime, DepScriptRuntime+".<name>")
	}
	caps, err := decodeCapabilities(in)
	if err != nil {
		return nil, err
	}

	node := scriptnode.NewNode(scriptNodeDeps(caps, runtimes.named))
	node.ValidateConfig = func(nodeID string, raw json.RawMessage) error {
		config, err := coregraph.DecodeConfig[scriptnode.ScriptConfig](raw)
		if err != nil {
			return errdefs.Validationf("graph node %q (type %q): %w", nodeID, typeName, err)
		}
		if _, ok := runtimes.named[config.Runtime]; !ok {
			return errdefs.NotFoundf(
				"graph node %q (type %q): script runtime %q is not wired (mounted: %s)",
				nodeID, typeName, config.Runtime, runtimes.list())
		}
		return nil
	}
	return &builtNodeType[scriptnode.ScriptConfig]{
		typeName:      typeName,
		fileRefFields: []string{"source"},
		node:          node,
	}, nil
}

// scanInferenceConfig reports what one inference node config asks of its
// collaborators: whether it pins a model itself, and which catalog tools it
// names statically. A value that still carries a ${board:*} reference is not
// static — the reference may resolve to a tool name, or replace the whole list,
// at invocation time — so it is counted as "uses tools" without being checked.
func scanInferenceConfig(raw json.RawMessage) (modelConfigured bool, staticTools []string, toolsConfigured bool, err error) {
	var fields map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return false, nil, false, errdefs.Validationf(
				"inference config is not a JSON object: %w", err)
		}
	}
	if model, ok := fields["model"]; ok && !bytes.Equal(bytes.TrimSpace(model), []byte("null")) {
		modelConfigured = true
	}
	tools, hasTools := fields["tools"]
	allTools := false
	if rawAll, ok := fields["all_tools"]; ok {
		if err := json.Unmarshal(rawAll, &allTools); err != nil {
			return false, nil, false, errdefs.Validationf(
				"inference config all_tools must be a boolean: %w", err)
		}
	}
	if !hasTools {
		return modelConfigured, nil, allTools, nil
	}
	if bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
		return modelConfigured, nil, allTools, nil
	}
	var names []string
	if err := json.Unmarshal(tools, &names); err != nil {
		// A board reference may replace the whole value at invocation time.
		return modelConfigured, nil, true, nil
	}
	for _, name := range names {
		if !strings.Contains(name, "${board:") {
			staticTools = append(staticTools, name)
		}
	}
	return modelConfigured, staticTools, len(names) > 0 || allTools, nil
}

// validateToolNames rejects a node naming a catalog tool that does not exist, so
// a typo fails the deployment instead of the node's first turn.
func validateToolNames(nodeID, typeName string, names []string, assembly *tool.Assembly) error {
	if len(names) == 0 || assembly == nil {
		return nil
	}
	available := make(map[string]struct{})
	for _, definition := range assembly.Catalog().Definitions() {
		available[definition.Name] = struct{}{}
	}
	for _, name := range names {
		if _, ok := available[name]; !ok {
			return errdefs.NotFoundf(
				"graph node %q (type %q): references unknown tool %q", nodeID, typeName, name)
		}
	}
	return nil
}

// scriptNodeDeps assembles the dependency bundle shared by the two
// script-running node types and the standard bindings surface. runtimes
// carries the script runtime(s) the type may select from; the standard
// bindings provider passes nil because it receives the executing runtime
// through the invocation.
func scriptNodeDeps(
	caps capabilities,
	runtimes map[string]agent.ScriptRuntime,
) scriptnode.ScriptNodeDeps {
	out := scriptnode.ScriptNodeDeps{
		Runtimes:      runtimes,
		Workspace:     caps.workspace,
		CommandRunner: caps.sandbox,
	}
	if caps.inference != nil {
		out.InferenceAssembly = caps.inference
	}
	if caps.router != nil {
		out.InferenceRouter = caps.router
	}
	if caps.tools != nil {
		out.ToolDispatcher = caps.tools
		out.ToolCatalog = caps.tools.Catalog()
	}
	return out
}
