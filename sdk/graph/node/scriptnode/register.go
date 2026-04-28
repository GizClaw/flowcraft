package scriptnode

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Deps captures the build-time dependencies needed to instantiate a
// scriptnode. ScriptRuntime is required; the rest are optional.
type Deps struct {
	ScriptRuntime script.Runtime
	ScriptFS      fs.FS
	Workspace     workspace.Workspace
	CommandRunner workspace.CommandRunner
}

// Built-in jsnode types that need a shell bridge in addition to fs/runtime.
var needsShellFS = map[string]bool{
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
	for _, name := range scripts.BuiltinTypes() {
		n := name
		factory.RegisterBuilder(n, func(def graph.NodeDefinition) (graph.Node, error) {
			if deps.ScriptRuntime == nil {
				return nil, errdefs.Validationf(
					"node %q (type %s): script runtime not configured", def.ID, n)
			}
			src := scripts.MustGet(n)
			var extras []bindings.BindingFunc
			if needsShellFS[n] {
				extras = append(extras, bindings.NewShellBridge(deps.CommandRunner))
			}
			extras = append(extras, bindings.NewFSBridge(deps.Workspace))
			return New(def.ID, n, src, def.Config, deps.ScriptRuntime, extras...), nil
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
		return New(def.ID, "script", source, def.Config, deps.ScriptRuntime, bindings.NewFSBridge(deps.Workspace)), nil
	})

	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		if deps.ScriptFS != nil {
			data, err := fs.ReadFile(deps.ScriptFS, def.Type+".js")
			if err == nil {
				if deps.ScriptRuntime == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): script runtime not configured", def.ID, def.Type)
				}
				return New(def.ID, def.Type, string(data), def.Config, deps.ScriptRuntime, bindings.NewFSBridge(deps.Workspace)), nil
			}
		}
		return nil, errdefs.Validationf(
			"unknown node type %q for node %q", def.Type, def.ID)
	})
}
