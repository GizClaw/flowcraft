package scriptnode

import (
	"io/fs"
	"reflect"

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
// sandbox.Runner rename) so existing host wiring does not have to
// chase the v0.2.0 rename. The field type is now sandbox.Runner;
// workspace.CommandRunner remains a working alias until v0.5.0.
type Deps struct {
	ScriptRuntime script.Runtime
	ScriptFS      fs.FS
	Workspace     workspace.Workspace
	CommandRunner sandbox.Runner
	EventBus      event.Bus
	ExtraBridges  []bindings.BindingFunc
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
	extraBridges := append([]bindings.BindingFunc(nil), deps.ExtraBridges...)
	newNode := func(id, nodeType, src string, config map[string]any, extras ...bindings.BindingFunc) *ScriptNode {
		nodeExtras := make([]bindings.BindingFunc, 0, len(extras)+len(extraBridges))
		nodeExtras = append(nodeExtras, extras...)
		nodeExtras = append(nodeExtras, extraBridges...)
		n := New(id, nodeType, src, config, deps.ScriptRuntime, nodeExtras...)
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
			if needsShell(n, def.Config) {
				if deps.CommandRunner == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): command runner not configured", def.ID, n)
				}
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

// needsShell reports whether this built-in's current configuration will call
// shell.exec. Files-only context nodes and commandless gates do not need a
// command runner at build time.
func needsShell(nodeType string, config map[string]any) bool {
	switch nodeType {
	case "context", "gate":
		return configValueNonEmpty(config, "commands")
	default:
		return false
	}
}

func configValueNonEmpty(config map[string]any, key string) bool {
	if config == nil {
		return false
	}
	v, ok := config[key]
	if !ok || v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() > 0
	default:
		return true
	}
}
