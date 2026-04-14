package graph

import (
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// CompiledCondition holds a pre-compiled expr-lang program for edge/skip conditions.
type CompiledCondition struct {
	Raw     string
	program *vm.Program
}

// CompileCondition compiles a raw expression string into a reusable program.
func CompileCondition(raw string) (*CompiledCondition, error) {
	program, err := expr.Compile(raw, expr.AsBool())
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"invalid condition expression: %s: %w", raw, err))
	}
	return &CompiledCondition{Raw: raw, program: program}, nil
}

// Evaluate runs the compiled condition against the given Board variables.
func (c *CompiledCondition) Evaluate(board *Board) (bool, error) {
	env := board.Vars()
	result, err := expr.Run(c.program, env)
	if err != nil {
		return false, fmt.Errorf(
			"condition eval failed: %s: %w", c.Raw, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf(
			"condition %q returned %T, expected bool", c.Raw, result)
	}
	return b, nil
}
