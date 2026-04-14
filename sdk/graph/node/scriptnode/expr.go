package scriptnode

import (
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

var exprCache sync.Map

func evalExpr(expression string, env map[string]any) (any, error) {
	if cached, ok := exprCache.Load(expression); ok {
		return expr.Run(cached.(*vm.Program), env)
	}
	program, err := expr.Compile(expression, expr.AllowUndefinedVariables())
	if err != nil {
		return nil, err
	}
	exprCache.Store(expression, program)
	return expr.Run(program, env)
}
