package scriptnode

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
)

// ScriptNode is a script-based graph node that delegates execution to a
// language-agnostic script.Runtime.
type ScriptNode struct {
	id          string
	nodeType    string
	script      string
	config      map[string]any
	runtime     script.Runtime
	extraBindFn []bindings.BindingFunc
	inputPorts  []graph.Port
	outputPorts []graph.Port
}

// New creates a ScriptNode with the given script and configuration.
func New(id, nodeType, scriptSrc string, config map[string]any, rt script.Runtime, extras ...bindings.BindingFunc) *ScriptNode {
	n := &ScriptNode{
		id:          id,
		nodeType:    nodeType,
		script:      scriptSrc,
		config:      config,
		runtime:     rt,
		extraBindFn: extras,
	}
	n.inputPorts, n.outputPorts = node.PortsForType(nodeType)
	if n.inputPorts == nil && n.outputPorts == nil {
		n.inputPorts = []graph.Port{{Name: "input", Type: graph.PortTypeAny}}
		n.outputPorts = []graph.Port{{Name: "output", Type: graph.PortTypeAny}}
	}
	return n
}

func (n *ScriptNode) ID() string                 { return n.id }
func (n *ScriptNode) Type() string               { return n.nodeType }
func (n *ScriptNode) InputPorts() []graph.Port   { return n.inputPorts }
func (n *ScriptNode) OutputPorts() []graph.Port  { return n.outputPorts }
func (n *ScriptNode) Config() map[string]any     { return n.config }
func (n *ScriptNode) SetConfig(c map[string]any) { n.config = c }

// ExecuteBoard runs the script with board, expr, host, stream, and
// runtime bindings.
func (n *ScriptNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	// Bridge wiring rationale:
	//
	//   - host: engine.Host control plane (publish / askUser / ...) plus
	//           the per-node stream channel via host.emit, fed by the
	//           executor-installed ctx.Publisher. Carries no identity
	//           accessors.
	//   - run:  agent.RunInfo identification bundle (per-run, immutable).
	//           Under direct graph execution only RunID is known, so
	//           the agent-layer fields (task / agent / context) are
	//           empty per bridge contract.
	//   - node: graph-layer per-step identity (id, type). Lives in
	//           scriptnode rather than in bindings because "node" is a
	//           graph concept that bindings deliberately does not know
	//           about (see sdk/script/bindings/doc.go).
	//
	// The node id passed to NewHostBridge is still used internally as
	// the askUser default source and for error annotations, but is no
	// longer surfaced as a script-readable accessor (use node.id()).
	allFns := []bindings.BindingFunc{
		bindings.NewBoardBridge(board),
		bindings.NewExprBridge(),
		bindings.NewHostBridge(ctx.Host, n.id, ctx.Publisher),
		bindings.NewRunInfoBridge(agent.RunInfo{RunID: ctx.RunID}),
		newNodeBridge(n.id, n.nodeType),
	}
	allFns = append(allFns, n.extraBindFn...)

	hostBindings := make(map[string]any, len(allFns)+1)
	for _, fn := range allFns {
		name, val := fn(ctx.Context)
		hostBindings[name] = val
	}
	hostBindings["runtime"] = bindings.RuntimeBinding(ctx.Context, n.runtime, hostBindings)

	env := &script.Env{
		Config:   n.config,
		Bindings: hostBindings,
	}

	sig, err := n.runtime.Exec(ctx.Context, n.id+".js", n.script, env)
	if err != nil {
		return fmt.Errorf("script node %s execution failed: %w", n.id, err)
	}

	if sig != nil {
		switch sig.Type {
		case "error":
			return fmt.Errorf("script node %s: %s", n.id, sig.Message)
		case "interrupt":
			// signal.interrupt(msg) is the script's "I want to pause,
			// the agent will resume me later". We surface it as a
			// CauseCustom interrupt so the agent layer can both detect
			// the pause (via errdefs.IsInterrupted) and read the
			// script-supplied detail.
			return engine.Interrupted(engine.Interrupt{
				Cause:  engine.CauseCustom,
				Detail: sig.Message,
			})
		}
	}

	return nil
}
