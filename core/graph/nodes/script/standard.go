package script

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// StandardImpl is the impl name of the standard script bindings
// provider registered as the "agent.ScriptBindings" deployment
// resource.
const StandardImpl = "standard"

// StandardBindings returns the standard script surface as a
// [bindings.Provider]: board, expr, host, run, tools, inference, node,
// stream, parallel, plus the opt-in fs and shell globals when deps wire
// a workspace or a sandbox runner, and the late "runtime" global for
// nested sub-scripts.
//
// The provider is immutable and safe to share: it captures the deps it
// was built with and reads per-execution state from
// [bindings.Invocation], so one value serves every script execution of
// every run.
func StandardBindings(deps ScriptNodeDeps) bindings.Provider {
	return standardBindings{deps: deps}
}

type standardBindings struct {
	deps ScriptNodeDeps
}

// Bind implements [bindings.Provider].
func (s standardBindings) Bind(inv bindings.Invocation) ([]bindings.Binding, error) {
	fns := []bindings.BindingFunc{
		bindings.NewBoardBridge(inv.Board),
		bindings.NewExprBridge(),
		bindings.NewHostBridge(inv.Host, inv.Name, scriptEmitter{
			nodeID: inv.NodeID,
			ctx:    inv.Context,
			emit:   inv.Emit,
		}),
		bindings.NewRunInfoBridge(),
		bindings.NewToolBridge(s.deps.ToolDispatcher, s.deps.ToolCatalog, s.deps.ToolOptions...),
		bindings.NewInferenceBridge(s.deps.InferenceAssembly, s.deps.InferenceRouter, s.deps.InferenceOptions...),
		newNodeBridge(inv.NodeID, inv.NodeType),
		newStreamBridge(inv.RunInfo.RunID, inv.Host),
		newParallelBridge(),
	}
	if s.deps.Workspace != nil {
		fns = append(fns, bindings.NewFSBridge(s.deps.Workspace, s.deps.FSOptions...))
	}
	if s.deps.CommandRunner != nil {
		fns = append(fns, bindings.NewShellBridge(s.deps.CommandRunner, s.deps.ShellOptions...))
	}

	out := make([]bindings.Binding, 0, len(fns)+1)
	for _, fn := range fns {
		name, value := fn(inv.Context)
		out = append(out, bindings.Binding{Name: name, Value: value})
	}
	return out, nil
}

// BindLate implements [bindings.LateProvider]: the "runtime" global
// wraps the runtime executing this invocation and captures the final
// bindings map, so nested sub-scripts inherit the same surface.
func (s standardBindings) BindLate(inv bindings.Invocation, env *agent.ScriptEnv) ([]bindings.Binding, error) {
	if inv.Runtime == nil {
		return nil, errdefs.NotAvailablef(
			"script bindings: the invocation carries no script runtime")
	}
	fn := bindings.NewRuntimeBridge(inv.Runtime)
	name, value := fn(inv.Context, env)
	return []bindings.Binding{{Name: name, Value: value}}, nil
}
