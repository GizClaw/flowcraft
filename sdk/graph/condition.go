package graph

import (
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// CompiledCondition is a pre-compiled expr-lang boolean program over
// board vars. Conditions are compiled once at [Build] time (edge
// conditions, skip conditions) and evaluated per wave with zero parse
// cost.
//
// The evaluation environment is Board.Vars() plus kernel-injected
// names (VarIterations): expressions reference board variables by
// name, e.g. `retrieved_docs != nil and len(retrieved_docs) > 0`.
type CompiledCondition struct {
	// Raw is the source expression, kept for diagnostics and
	// checkpoint inspection.
	Raw string

	program *vm.Program
}

// compileCondition parses raw into a reusable boolean program.
func compileCondition(raw string) (*CompiledCondition, error) {
	program, err := expr.Compile(raw, expr.AsBool())
	if err != nil {
		return nil, errdefs.Validationf("invalid condition expression %q: %v", raw, err)
	}
	return &CompiledCondition{Raw: raw, program: program}, nil
}

// Evaluate runs the condition against the board's current vars.
func (c *CompiledCondition) Evaluate(board *agent.Board) (bool, error) {
	return c.evaluate(board.Vars())
}

// evaluate runs the condition against a pre-assembled environment.
func (c *CompiledCondition) evaluate(env map[string]any) (bool, error) {
	out, err := expr.Run(c.program, env)
	if err != nil {
		return false, errdefs.Validationf("condition %q evaluation failed: %v", c.Raw, err)
	}
	result, ok := out.(bool)
	if !ok {
		return false, errdefs.Validationf("condition %q did not evaluate to a boolean", c.Raw)
	}
	return result, nil
}
