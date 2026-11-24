package layout_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/layout"
)

// TestMetricLabelsCarryNoIdentifiers is the metric half of the cardinality rule: a series whose
// labels include a conversation, a task, a turn or a call id becomes one series per record, which
// is a metrics store full of single-sample histograms and a dashboard nobody can group.
//
// It is a red line rather than a census. The scan resolves every attribute key it can see in a file
// that creates a metric instrument — string literals, and identifiers declared anywhere in the tree
// (`identity.AttrSessionID`, `telemetry.AttrAgentState`) — and fails when the resolved key names one
// of the identifiers below. A key it cannot resolve is not proven safe, only unproven; the allowlist
// discipline lives in the per-package tests that assert what each series carries.
func TestMetricLabelsCarryNoIdentifiers(t *testing.T) {
	forbidden := map[string]string{
		"session.id":         "one series per conversation",
		"task.id":            "one series per turn",
		"turn":               "one series per turn",
		"tool.call_id":       "one series per call",
		"chain.id":           "one series per delegation chain",
		"chain.depth":        "an identifier by another name",
		"agent.authority.id": "one series per grant",
		"session.bind.id":    "one series per narrowed grant",
		"error.message":      "unbounded text, and a payload besides",
		"code.stacktrace":    "unbounded text",
	}

	root, err := layout.Root()
	if err != nil {
		t.Fatalf("layout.Root: %v", err)
	}
	// Sibling modules emit metrics too (the tool chain, the inference pipeline), and the rule is a
	// framework one, so the scan starts at the workspace rather than at the module.
	workspace := filepath.Dir(root)
	consts := stringConstants(t, workspace)

	offenders := 0
	err = filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != workspace && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "core-tmp") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		if !createsMetrics(file) {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return relErr
		}
		for _, key := range metricLabelKeys(file, consts) {
			if why, bad := forbidden[key]; bad {
				offenders++
				t.Errorf("%s: metric label %q (%s); put it on a span instead", filepath.ToSlash(rel), key, why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan for metric labels: %v", err)
	}
	if offenders == 0 {
		// The scan is only meaningful if it found attribute calls at all: a refactor that renames
		// the constructors would otherwise turn this test into a no-op.
		if len(consts) == 0 {
			t.Fatal("no string constants were indexed; the scan cannot resolve any attribute key")
		}
	}
}

// metricLabelKeys collects the attribute keys a file hands to `metric.WithAttributes`.
//
// Only the label *positions* count: a file that emits metrics also puts `tool.call_id` on a span and
// `error.message` in a log line, and neither is a cardinality problem. Label values are resolved
// through a local assignment map, which is what catches the two shapes this tree uses — a direct
// `metric.WithAttributes(attribute.String(key, v))` and a slice built once and passed positionally.
func metricLabelKeys(file *ast.File, consts map[string]string) []string {
	assigned := map[string][]string{}
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			keys := keysInExpr(rhs, consts, assigned)
			if len(keys) == 0 || i >= len(assign.Lhs) {
				continue
			}
			if name, ok := assign.Lhs[i].(*ast.Ident); ok {
				assigned[name.Name] = append(assigned[name.Name], keys...)
			}
		}
		return true
	})

	var keys []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WithAttributes" {
			return true
		}
		if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "metric" {
			return true
		}
		for _, arg := range call.Args {
			keys = append(keys, keysInExpr(arg, consts, assigned)...)
		}
		return true
	})
	return keys
}

// keysInExpr resolves the attribute keys one label expression contributes, if any are knowable.
func keysInExpr(expr ast.Expr, consts map[string]string, assigned map[string][]string) []string {
	switch typed := expr.(type) {
	case *ast.CallExpr:
		if key, ok := attributeKey(typed, consts); ok {
			return []string{key}
		}
		// append(attrs, extra...) is how the pipeline stacks its shared set; follow the literal
		// half and the named half.
		if selector, ok := typed.Fun.(*ast.Ident); ok && selector.Name == "append" {
			var keys []string
			for _, arg := range typed.Args {
				keys = append(keys, keysInExpr(arg, consts, assigned)...)
			}
			return keys
		}
		return nil
	case *ast.Ident:
		return assigned[typed.Name]
	case *ast.SelectorExpr:
		return assigned[typed.Sel.Name]
	case *ast.CompositeLit:
		var keys []string
		for _, element := range typed.Elts {
			keys = append(keys, keysInExpr(element, consts, assigned)...)
		}
		return keys
	default:
		return nil
	}
}

// createsMetrics reports whether a file builds a metric instrument.
func createsMetrics(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Int64Counter", "Float64Counter", "Int64Histogram", "Float64Histogram",
			"Int64UpDownCounter", "Float64UpDownCounter", "Int64Gauge", "Float64Gauge",
			"Int64ObservableCounter", "Float64ObservableCounter", "Int64ObservableGauge",
			"Float64ObservableGauge", "Int64ObservableUpDownCounter":
			found = true
			return false
		}
		return true
	})
	return found
}

// attributeKey resolves the first argument of an attribute constructor to the key it names.
//
// Both spellings are in use: the OpenTelemetry `attribute.String(key, value)` and this framework's
// `telemetry.String(key, value)` alias. Anything else — a slice, a variable, `WithAttributes` itself
// — carries no key of its own and is skipped.
func attributeKey(call *ast.CallExpr, consts map[string]string) (string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	switch selector.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Float64", "StringSlice":
	default:
		return "", false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || (pkg.Name != "attribute" && pkg.Name != "telemetry") {
		return "", false
	}
	return resolveKey(call.Args[0], consts)
}

// resolveKey turns a key expression into its value, when the value is knowable statically.
func resolveKey(expr ast.Expr, consts map[string]string) (string, bool) {
	switch typed := expr.(type) {
	case *ast.BasicLit:
		if typed.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(typed.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.Ident:
		value, ok := consts[typed.Name]
		return value, ok
	case *ast.SelectorExpr:
		value, ok := consts[typed.Sel.Name]
		return value, ok
	default:
		return "", false
	}
}

// stringConstants indexes every package-level string constant in the workspace by its name, which is
// enough to resolve both `AttrSessionID` and `identity.AttrSessionID`.
func stringConstants(t *testing.T, workspace string) map[string]string {
	t.Helper()
	consts := map[string]string{}
	err := filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != workspace && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "core-tmp") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := stringConstValue(spec)
				if !ok {
					continue
				}
				for _, name := range spec.(*ast.ValueSpec).Names {
					consts[name.Name] = value
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("index string constants: %v", err)
	}
	return consts
}

// stringConstValue returns the value of one const spec's single string literal, if it has one.
func stringConstValue(spec ast.Spec) (string, bool) {
	value, ok := spec.(*ast.ValueSpec)
	if !ok || len(value.Values) != 1 || len(value.Names) != 1 {
		return "", false
	}
	literal, ok := value.Values[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	unquoted, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return unquoted, true
}
