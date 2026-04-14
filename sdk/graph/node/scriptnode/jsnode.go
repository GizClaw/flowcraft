package scriptnode

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script"
)

// ScriptNode is a script-based graph node that delegates execution to a
// language-agnostic script.Runtime.
type ScriptNode struct {
	id          string
	nodeType    string
	script      string
	config      map[string]any
	runtime     script.Runtime
	extraBindFn []BindingFunc
	inputPorts  []graph.Port
	outputPorts []graph.Port
}

// New creates a ScriptNode with the given script and configuration.
func New(id, nodeType, scriptSrc string, config map[string]any, rt script.Runtime, extras ...BindingFunc) *ScriptNode {
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

// ExecuteBoard runs the script with board, expr, stream, and runtime bindings.
func (n *ScriptNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	allFns := []BindingFunc{
		NewBoardBridge(board),
		NewExprBridge(),
		NewStreamBridge(ctx.Stream, n.id),
	}
	allFns = append(allFns, n.extraBindFn...)

	bindings := make(map[string]any, len(allFns)+1)
	for _, fn := range allFns {
		name, val := fn(ctx.Context)
		bindings[name] = val
	}
	bindings["runtime"] = runtimeBindings(ctx.Context, n.runtime, bindings)

	env := &script.Env{
		Config:   n.config,
		Bindings: bindings,
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
			return graph.ErrInterrupt
		}
	}

	return nil
}
