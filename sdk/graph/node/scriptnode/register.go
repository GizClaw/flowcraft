package scriptnode

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
)

var needsShellFS = map[string]bool{
	"gate":    true,
	"context": true,
}

func init() {
	for _, name := range scripts.BuiltinTypes() {
		n := name
		node.RegisterDefaultBuilder(n, func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
			if bctx.ScriptRuntime == nil {
				return nil, errdefs.Validationf(
					"node %q (type %s): script runtime not configured", def.ID, n)
			}
			src := scripts.MustGet(n)
			var extra []bindings.BindingFunc
			if needsShellFS[n] {
				extra = append(extra, bindings.NewShellBridge(bctx.CommandRunner))
			}
			extra = append(extra, bindings.NewFSBridge(bctx.Workspace))
			return New(def.ID, n, src, def.Config, bctx.ScriptRuntime, extra...), nil
		})
	}

	node.RegisterDefaultBuilder("script", func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		if bctx.ScriptRuntime == nil {
			return nil, errdefs.Validationf(
				"node %q (type script): script runtime not configured", def.ID)
		}
		source, _ := def.Config["source"].(string)
		if source == "" {
			return nil, errdefs.Validationf(
				"node %q (type script): config.source is required", def.ID)
		}
		return New(def.ID, "script", source, def.Config, bctx.ScriptRuntime, bindings.NewFSBridge(bctx.Workspace)), nil
	})

	node.RegisterFallbackBuilder(func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		if bctx.ScriptFS != nil {
			data, err := fs.ReadFile(bctx.ScriptFS, def.Type+".js")
			if err == nil {
				if bctx.ScriptRuntime == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): script runtime not configured", def.ID, def.Type)
				}
				return New(def.ID, def.Type, string(data), def.Config, bctx.ScriptRuntime, bindings.NewFSBridge(bctx.Workspace)), nil
			}
		}
		return nil, errdefs.Validationf(
			"unknown node type %q for node %q", def.Type, def.ID)
	})
}
