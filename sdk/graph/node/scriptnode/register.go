package scriptnode

import (
	"errors"
	"io/fs"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type scriptFallbackOwner struct{}

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
	ExtraBridges  []script.BindingFunc
}

// Register binds the script-backed node builders ("script", every built-in
// declared by the scriptnode builtin catalog, plus a fallback that resolves
// type.js from deps.ScriptFS) onto factory.
//
// deps.ScriptRuntime is required at build time for any node this function
// registers; nodes whose type also needs a shell bridge will additionally
// require deps.CommandRunner. Missing deps are reported as a build-time
// validation error from Factory.Build.
func Register(factory *node.Factory, deps Deps) {
	extraBridges := append([]script.BindingFunc(nil), deps.ExtraBridges...)
	newNode := func(id, nodeType, src string, config map[string]any, extras ...script.BindingFunc) *ScriptNode {
		nodeExtras := make([]script.BindingFunc, 0, len(extras)+len(extraBridges))
		nodeExtras = append(nodeExtras, extras...)
		nodeExtras = append(nodeExtras, extraBridges...)
		n := New(id, nodeType, src, config, deps.ScriptRuntime, nodeExtras...)
		n.eventBus = deps.EventBus
		return n
	}

	for _, spec := range builtinCatalog {
		spec := spec
		factory.RegisterBuilder(spec.Type(), func(def graph.NodeDefinition) (graph.Node, error) {
			if deps.ScriptRuntime == nil {
				return nil, errdefs.Validationf(
					"node %q (type %s): script runtime not configured", def.ID, spec.Type(),
				)
			}
			src, err := spec.Source()
			if err != nil {
				return nil, errdefs.Internalf(
					"node %q (type %s): builtin script source not configured: %v", def.ID, spec.Type(), err,
				)
			}
			var extras []script.BindingFunc
			if spec.needsCommandRunner(def.Config) {
				if deps.CommandRunner == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): command runner not configured", def.ID, spec.Type(),
					)
				}
				extras = append(extras, bindings.NewShellBridge(deps.CommandRunner))
			}
			extras = append(extras, spec.defaultBridges(deps)...)
			return newNode(def.ID, spec.Type(), src, def.Config, extras...), nil
		})
	}

	factory.RegisterBuilder("script", func(def graph.NodeDefinition) (graph.Node, error) {
		if deps.ScriptRuntime == nil {
			return nil, errdefs.Validationf(
				"node %q (type script): script runtime not configured", def.ID,
			)
		}
		source, _ := def.Config["source"].(string)
		if source == "" {
			return nil, errdefs.Validationf(
				"node %q (type script): config.source is required", def.ID,
			)
		}
		return newNode(def.ID, "script", source, def.Config, bindings.NewFSBridge(deps.Workspace)), nil
	})

	oldFallback := externalFallbackFor(factory)
	newFallback := func(def graph.NodeDefinition) (graph.Node, error) {
		if deps.ScriptFS != nil {
			sourcePath := def.Type + ".js"
			data, err := fs.ReadFile(deps.ScriptFS, sourcePath)
			if err == nil {
				if deps.ScriptRuntime == nil {
					return nil, errdefs.Validationf(
						"node %q (type %s): script runtime not configured", def.ID, def.Type,
					)
				}
				return newNode(def.ID, def.Type, string(data), def.Config, bindings.NewFSBridge(deps.Workspace)), nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, errdefs.Internalf(
					"node %q (type %s): read script source %q: read error: %v",
					def.ID, def.Type, sourcePath, err,
				)
			}
		}
		// Preserve any caller-provided fallback after ScriptFS has no match.
		if oldFallback != nil {
			return oldFallback(def)
		}
		return nil, errdefs.Validationf(
			"unknown node type %q for node %q", def.Type, def.ID,
		)
	}
	factory.SetFallback(newFallback)
	rememberScriptFallback(factory, oldFallback, newFallback)
}

func externalFallbackFor(factory *node.Factory) node.NodeBuilder {
	current := factory.Fallback()
	if registered, ok := factory.FallbackRegistration(scriptFallbackOwner{}); ok {
		if current != nil && nodeBuilderPC(current) == registered.InstalledPC {
			return registered.External
		}
	}
	return current
}

func rememberScriptFallback(factory *node.Factory, external, installed node.NodeBuilder) {
	factory.SetFallbackRegistration(scriptFallbackOwner{}, node.FallbackRegistration{
		External:    external,
		InstalledPC: nodeBuilderPC(installed),
	})
}

func nodeBuilderPC(builder node.NodeBuilder) uintptr {
	if builder == nil {
		return 0
	}
	return reflect.ValueOf(builder).Pointer()
}
