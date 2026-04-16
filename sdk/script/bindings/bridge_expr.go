package bindings

import "context"

// NewExprBridge exposes expr-lang as global "expr" (eval).
func NewExprBridge() BindingFunc {
	return func(_ context.Context) (string, any) {
		return "expr", map[string]any{
			"eval": func(expression string, env map[string]any) (any, error) {
				return evalExpr(expression, env)
			},
		}
	}
}
