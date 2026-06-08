package scriptnode

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Deps captures the build-time dependencies needed to instantiate a
// scriptnode. ScriptRuntime is required; the rest are optional.
//
// CommandRunner keeps its field name (rather than tracking the
// sandbox.Runner rename) so existing wiring in callers like vesseld
// does not have to chase the v0.2.0 rename. The field type is now
// sandbox.Runner; workspace.CommandRunner remains a working alias until
// v0.5.0.
type Deps struct {
	ScriptRuntime script.Runtime
	ScriptFS      fs.FS
	Workspace     workspace.Workspace
	CommandRunner sandbox.Runner
	EventBus      event.Bus
}

// needsShell lists built-in jsnode types that additionally require a
// shell bridge. Every built-in node already gets fs / board / expr /
// host / runtime bridges unconditionally below; the entries here are
// the *additional* shell bridge gate (driven by deps.CommandRunner).
var needsShell = map[string]bool{
	"gate":    true,
	"context": true,
}

// Register binds the built-in script-backed node builders ("script", every
// type in scripts.BuiltinTypes(), plus a fallback that resolves
// type.js from deps.ScriptFS) onto factory.
//
// deps.ScriptRuntime is required at build time for any node this function
// registers; nodes whose type also needs a shell bridge will additionally
// require deps.CommandRunner. Missing deps are reported as a build-time
// validation error from Factory.Build.
func Register(factory *node.Factory, deps Deps) {
	newNode := func(id, nodeType, src string, config map[string]any, extras ...bindings.BindingFunc) *ScriptNode {
		n := New(id, nodeType, src, config, deps.ScriptRuntime, extras...)
		n.eventBus = deps.EventBus
		return n
	}

	for _, name := range scripts.BuiltinTypes() {
		n := name
		factory.RegisterBuilder(n, func(def graph.NodeDefinition) (graph.Node, error) {
			if deps.ScriptRuntime == nil {
				return nil, errdefs.Validationf(
					"node %q (type %s): script runtime not configured", def.ID, n)
			}
			src := scripts.MustGet(n)
			var extras []bindings.BindingFunc
			if needsShell[n] {
				extras = append(extras, bindings.NewShellBridge(deps.CommandRunner))
			}
			extras = append(extras, bindings.NewFSBridge(deps.Workspace))
			return newNode(def.ID, n, src, def.Config, extras...), nil
		})
	}

	factory.RegisterBuilder("script", func(def graph.NodeDefinition) (graph.Node, error) {
		if deps.ScriptRuntime == nil {
			return nil, errdefs.Validationf(
				"node %q (type script): script runtime not configured", def.ID)
		}
		source, _ := def.Config["source"].(string)
		if source == "" {
			return nil, errdefs.Validationf(
				"node %q (type script): config.source is required", def.ID)
		}
		return newNode(def.ID, "script", source, def.Config, bindings.NewFSBridge(deps.Workspace)), nil
	})

	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		if deps.ScriptFS != nil {
			data, err := fs.ReadFile(deps.ScriptFS, def.Type+".js")
			if err == nil {
				if deps.ScriptRuntime == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): script runtime not configured", def.ID, def.Type)
				}
				return newNode(def.ID, def.Type, string(data), def.Config, bindings.NewFSBridge(deps.Workspace)), nil
			}
		}
		return nil, errdefs.Validationf(
			"unknown node type %q for node %q", def.Type, def.ID)
	})
}
