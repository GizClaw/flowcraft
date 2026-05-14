package taubench

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
)

// Sierra τ-bench tool ports — common helpers
//
// Sierra's Python tools (sierra-research/tau-bench, commit
// 59a200c6, dated 2026-03-18) all follow the same wire contract:
//
//   - State is a single map (initial state from data/*.json merged
//     under top-level keys like "orders" / "users" / "products").
//   - Each tool takes typed kwargs from the LLM.
//   - Each tool returns a STRING — either json.dumps(<some object>)
//     on success, or "Error: ..." on a domain-rule violation (note:
//     not a Python exception; the LLM agent reads this and retries).
//   - State mutations are in-place on `data` (we pass our State map
//     by reference, same).
//
// We deliberately keep these helpers tight: typed arg extraction,
// JSON-string return helper, a safe arithmetic evaluator (Sierra's
// retail/calculate.py uses Python's `eval` with a tiny char-allowlist;
// we use go/parser to walk the AST instead — same semantics, no
// eval, no recursion-depth risk).

// argString reads args[name] as a string. Sierra invariably sends
// string keys; if the LLM serialised a number as JSON we surface a
// typed error so the harness fails loud instead of silently
// stringifying nonsense.
func argString(args map[string]any, name string) (string, error) {
	v, ok := args[name]
	if !ok {
		return "", fmt.Errorf("missing argument %q", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q: expected string, got %T", name, v)
	}
	return s, nil
}

// argStringList reads args[name] as []string. Accepts a Go []string
// (rare; only from native test fixtures) or a JSON-shaped []any
// where every element is a string (the normal LLM path).
func argStringList(args map[string]any, name string) ([]string, error) {
	v, ok := args[name]
	if !ok {
		return nil, fmt.Errorf("missing argument %q", name)
	}
	switch xs := v.(type) {
	case []string:
		return xs, nil
	case []any:
		out := make([]string, len(xs))
		for i, x := range xs {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("argument %q[%d]: expected string, got %T", name, i, x)
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argument %q: expected []string, got %T", name, v)
	}
}

// asMap is the unchecked downcast we apply when State is structured
// (Sierra's initial_state.json is rigorously typed; this helper
// exists only to keep call sites readable). Missing key → nil map.
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// asSlice mirrors asMap for arrays.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// asFloat returns the underlying numeric value for a JSON-decoded
// `any`. encoding/json decodes JSON numbers as float64; some
// fixtures hand-write int literals — we accept both.
func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// round2 mirrors Python's round(x, 2). Sierra rounds every
// gift-card / price-difference computation this way; deviating
// produces an off-by-one-cent state delta that fails the
// ExpectedFinalState DeepEqual.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// jsonString is the Go analogue of Python's json.dumps. Tools
// return this on the success path so the Hits string mirrors what
// Sierra emits at the equivalent action.
func jsonString(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// safeEval evaluates a Sierra `calculate(...)` expression. Sierra's
// own implementation passes the string to Python's `eval()` after a
// char-allowlist check ("0123456789+-*/(). "). We replicate the
// semantics by parsing the expression with go/parser (which handles
// precedence + parens identically to Python for +,-,*,/) and walking
// the BasicLit / BinaryExpr / ParenExpr / UnaryExpr nodes.
//
// Anything outside this grammar (function calls, identifiers, octal
// literals starting with 0o, etc.) returns an error mirroring
// Sierra's "Error: invalid characters in expression" branch.
func safeEval(expr string) (float64, error) {
	for _, r := range expr {
		switch {
		case r >= '0' && r <= '9':
		case r == '+' || r == '-' || r == '*' || r == '/':
		case r == '(' || r == ')' || r == '.' || r == ' ':
		default:
			return 0, fmt.Errorf("invalid characters in expression")
		}
	}
	tree, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, err
	}
	return evalNode(tree)
}

func evalNode(n ast.Node) (float64, error) {
	switch x := n.(type) {
	case *ast.BasicLit:
		if x.Kind != token.INT && x.Kind != token.FLOAT {
			return 0, fmt.Errorf("invalid literal kind %v", x.Kind)
		}
		f, err := strParseFloat(x.Value)
		if err != nil {
			return 0, err
		}
		return f, nil
	case *ast.ParenExpr:
		return evalNode(x.X)
	case *ast.UnaryExpr:
		v, err := evalNode(x.X)
		if err != nil {
			return 0, err
		}
		switch x.Op {
		case token.ADD:
			return v, nil
		case token.SUB:
			return -v, nil
		}
		return 0, fmt.Errorf("unsupported unary op %s", x.Op)
	case *ast.BinaryExpr:
		l, err := evalNode(x.X)
		if err != nil {
			return 0, err
		}
		r, err := evalNode(x.Y)
		if err != nil {
			return 0, err
		}
		switch x.Op {
		case token.ADD:
			return l + r, nil
		case token.SUB:
			return l - r, nil
		case token.MUL:
			return l * r, nil
		case token.QUO:
			if r == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return l / r, nil
		}
		return 0, fmt.Errorf("unsupported binary op %s", x.Op)
	}
	return 0, fmt.Errorf("unsupported expression node %T", n)
}

func strParseFloat(s string) (float64, error) {
	// strconv to avoid pulling in "strconv" via a top-level import
	// when only this helper uses it — but go fmt prefers an import.
	// Just import strconv at top; this isolated helper keeps the
	// node-walk readable.
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	if err != nil {
		return 0, err
	}
	return f, nil
}

// countString returns the number of occurrences of s in xs. Sierra
// uses list.count() in several tools to validate that duplicate
// item ids in the action's item_ids list don't exceed the
// duplicate count in the order; we mirror it bit-for-bit.
func countString(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}
