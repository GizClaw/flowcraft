package layout_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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
// It is a red line rather than a census. The scan reads the label positions of every file that
// creates a metric instrument and resolves each key it can statically: string literals, identifiers
// declared anywhere in the tree (`identity.AttrSessionID`, `telemetry.AttrAgentState`), the local
// variable a set was built in, the appends that grew it, and a same-file helper that returns a
// label option. It fails when a resolved key names one of the identifiers below.
//
// A key it cannot resolve is not proven safe, only unproven, and silence was this test's old answer
// to that. It is a decision now: an unresolvable key must be named in unresolvedKeys with the reason
// it cannot be one of the identifiers below, so a new dynamic label is something a reviewer sees
// rather than something a scan skips.
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
	labels := 0
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
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
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
		where := filepath.ToSlash(filepath.Dir(rel))
		keys := metricLabelKeys(file, consts)
		labels += len(keys)
		for _, label := range keys {
			if !label.resolved {
				key := where + ": " + label.expr
				if why, ok := unresolvedKeys[key]; ok {
					t.Logf("%s: unresolvable metric label %q allowed (%s)", filepath.ToSlash(rel), label.expr, why)
					continue
				}
				offenders++
				t.Errorf("%s: metric label key %q cannot be resolved statically, so this scan cannot "+
					"prove it is low-cardinality; make the key a constant or literal, or name the "+
					"expression %q in unresolvedKeys with the reason it is safe",
					filepath.ToSlash(rel), label.expr, key)
				continue
			}
			if why, bad := forbidden[label.key]; bad {
				offenders++
				t.Errorf("%s: metric label %q (%s); put it on a span instead", filepath.ToSlash(rel), label.key, why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan for metric labels: %v", err)
	}
	if offenders == 0 {
		// The scan is only meaningful if it found label positions at all: a refactor that renames
		// the constructors would otherwise turn this test into a no-op.
		if len(consts) == 0 {
			t.Fatal("no string constants were indexed; the scan cannot resolve any attribute key")
		}
		if labels == 0 {
			t.Fatal("no metric label positions were found; the scan reads metric.WithAttributes, " +
				"so a rename of the constructors leaves this test passing on nothing")
		}
	}
}

// unresolvedKeys is the allowlist of label positions the scan cannot read, indexed by
// "<package dir relative to the workspace>: <source text of the key expression>".
//
// It is empty on purpose. The three entries this guard first reported were all one imprecision —
// the scan read one file-wide variable table, so two functions that both named a variable `attrs`
// were the same set — and widening the scan to follow a same-file helper was the fix, not an
// allowlist line. Keep it that way: a new entry is a claim that a *dynamic* label set cannot become
// an identifier, and the reviewer of the pull request is the one who has to believe it.
var unresolvedKeys = map[string]string{}

// label is one attribute-key position, resolved or not.
//
// resolved is what makes the difference between a scan and a claim: a key this test cannot read
// statically is a position where a cardinality rule could be broken invisibly, so it is reported
// rather than skipped.
type label struct {
	// key is the attribute key, when key is knowable statically.
	key string
	// expr is the source text of the key expression, the string unresolvedKeys is indexed by.
	expr string
	// resolved reports whether key holds a value.
	resolved bool
}

// metricLabelKeys collects the attribute keys a file hands to `metric.WithAttributes`.
//
// Only the label *positions* count: a file that emits metrics also puts `tool.call_id` on a span and
// `error.message` in a log line, and neither is a cardinality problem.
//
// The file is read one function at a time. A single variable table for the whole file was wrong:
// every function that builds its label set in a variable called `attrs` then shares that variable
// with every other one, which is how a usage attribute set from one method was reported as a metric
// label of another.
func metricLabelKeys(file *ast.File, consts map[string]string) []label {
	helpers := fileHelpers(file, consts)
	var keys []label
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		keys = append(keys, functionLabelKeys(fn, consts, helpers)...)
	}
	return dedupeLabels(keys)
}

// functionLabelKeys reads one function body: the local sets its assignments build, then every
// metric.WithAttributes call site in it.
func functionLabelKeys(fn *ast.FuncDecl, consts map[string]string, helpers map[string][]label) []label {
	scope := &labelScope{
		consts:   consts,
		params:   paramNames(fn),
		assigned: map[string][]label{},
		helpers:  helpers,
		// A helper may call another helper, and a self-call must terminate. One function is
		// visited at most once per file: a second visit would contribute nothing new.
		visiting: map[string]bool{},
	}

	// The assignment pass runs first, so a set built after its first use is still resolved at the
	// call site that reads it.
	scope.silent = true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if i >= len(assign.Lhs) {
				continue
			}
			name, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			keys := keysInExpr(rhs, scope)
			if len(keys) == 0 {
				continue
			}
			scope.assigned[name.Name] = dedupeLabels(append(scope.assigned[name.Name], keys...))
		}
		return true
	})
	scope.silent = false

	var keys []label
	ast.Inspect(fn.Body, func(node ast.Node) bool {
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
			keys = append(keys, keysInExpr(arg, scope)...)
		}
		return true
	})
	return keys
}

// labelScope is the resolution context of one function body.
type labelScope struct {
	// consts indexes the workspace's package-level string constants, by name.
	consts map[string]string
	// params are the enclosing function's parameter names. A parameter's labels arrive at the
	// call site, not here.
	params map[string]bool
	// assigned maps one local variable to the label keys its assignments carry.
	assigned map[string][]label
	// helpers maps a same-file function that returns a label option to the keys it carries.
	helpers map[string][]label
	// visiting guards recursive helper resolution.
	visiting map[string]bool
	// silent suppresses an unresolved report while a label set is still being built. An
	// assignment like `attrs = append(attrs, x)` reads `attrs` before the pass that records
	// it has finished, so the build phase must not treat "not seen yet" as "not knowable":
	// the consumption site — a WithAttributes call, or a helper's return — is where the
	// question is asked, and it is asked with the finished table.
	silent bool
}

// paramNames collects the parameter names of one function, including a variadic tail.
func paramNames(fn *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	if fn.Type == nil || fn.Type.Params == nil {
		return names
	}
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			names[name.Name] = true
		}
	}
	return names
}

// fileHelpers reads every function in the file that returns a label option, so a call to it resolves
// to the keys it carries instead of ending the scan.
//
// This is what lets `metric.WithAttributes(usageAttrs(tokens)...)` and a method like
// `c.metricAttrs(extra)` be checked: the helper's own constants are read here, its parameters are
// left for the call site, and an argument the caller adds is read there.
func fileHelpers(file *ast.File, consts map[string]string) map[string][]label {
	helpers := map[string][]label{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name == nil {
			continue
		}
		keys := returnedLabelKeys(fn, consts)
		if len(keys) > 0 {
			helpers[fn.Name.Name] = keys
		}
	}
	return helpers
}

// returnedLabelKeys reads the label keys a function returns, which is the body of a helper that
// builds a metric.MeasurementOption. Parameters are skipped: they are the caller's labels.
func returnedLabelKeys(fn *ast.FuncDecl, consts map[string]string) []label {
	scope := &labelScope{
		consts:   consts,
		params:   paramNames(fn),
		assigned: map[string][]label{},
		helpers:  map[string][]label{},
		visiting: map[string]bool{},
		silent:   true,
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if i >= len(assign.Lhs) {
				continue
			}
			name, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			keys := keysInExpr(rhs, scope)
			if len(keys) == 0 {
				continue
			}
			scope.assigned[name.Name] = dedupeLabels(append(scope.assigned[name.Name], keys...))
		}
		return true
	})
	scope.silent = false

	var keys []label
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, result := range ret.Results {
			keys = append(keys, keysInExpr(result, scope)...)
		}
		return true
	})
	return dedupeLabels(keys)
}

// keysInExpr resolves the attribute keys one label expression contributes.
//
// An attribute constructor whose key argument is not a literal or a constant yields one unresolved
// label: the caller decides whether that is a decision or a defect, and this function's job is to
// make sure it is never silently dropped.
func keysInExpr(expr ast.Expr, scope *labelScope) []label {
	switch typed := expr.(type) {
	case *ast.CallExpr:
		if key, ok := attributeKey(typed, scope.consts); ok {
			return []label{{key: key, resolved: true}}
		}
		selector, isSelector := typed.Fun.(*ast.SelectorExpr)
		ident, isIdent := typed.Fun.(*ast.Ident)
		switch {
		// make([]attribute.KeyValue, 0, 8) is an empty label set: the appends that follow are
		// what fill it, and each of those is recorded under the name it is assigned to.
		case isIdent && ident.Name == "make":
			return nil
		// append(attrs, extra...) is how every package here stacks its shared set; follow the
		// literal half and the named half.
		case isIdent && ident.Name == "append":
			var keys []label
			for _, arg := range typed.Args {
				keys = append(keys, keysInExpr(arg, scope)...)
			}
			return keys
		// A prebuilt option — `attrs := metric.WithAttributes(...)`, then
		// `WithAttributes(attrs)` at the call site — is how three packages stack a label set they
		// assembled conditionally.
		case isSelector && selector.Sel.Name == "WithAttributes":
			var keys []label
			for _, arg := range typed.Args {
				keys = append(keys, keysInExpr(arg, scope)...)
			}
			return keys
		// A same-file helper: its own keys, plus whatever this call adds.
		case isIdent && scope.helpers[ident.Name] != nil:
			return append(helperLabels(ident.Name, scope), argLabels(typed, scope)...)
		case isSelector && scope.helpers[selector.Sel.Name] != nil:
			return append(helperLabels(selector.Sel.Name, scope), argLabels(typed, scope)...)
		case isSelector && isAttributeConstructor(selector):
			// An attribute constructor this scan reached: the key is the first argument, whatever
			// it turns out to be.
			if len(typed.Args) == 0 {
				return nil
			}
			return []label{{expr: types.ExprString(typed.Args[0])}}
		}
		// Any other call contributes a label set this scan cannot read. It is reported, not
		// dropped: a helper that builds keys from its arguments is exactly how a cardinality rule
		// is lost.
		return scope.unresolved(types.ExprString(typed))
	case *ast.Ident:
		if scope.params[typed.Name] {
			return nil
		}
		if keys, ok := scope.assigned[typed.Name]; ok {
			return keys
		}
		return scope.unresolved(typed.Name)
	case *ast.SelectorExpr:
		if keys, ok := scope.assigned[typed.Sel.Name]; ok {
			return keys
		}
		return scope.unresolved(types.ExprString(typed))
	case *ast.CompositeLit:
		var keys []label
		for _, element := range typed.Elts {
			keys = append(keys, keysInExpr(element, scope)...)
		}
		return keys
	default:
		return nil
	}
}

// unresolved reports a label position this scan could not read, unless the set is still being
// built.
func (s *labelScope) unresolved(expr string) []label {
	if s.silent {
		return nil
	}
	return []label{{expr: expr}}
}

// helperLabels returns a same-file helper's keys, once per call: a helper that calls itself would
// otherwise recurse until the stack ran out.
func helperLabels(name string, scope *labelScope) []label {
	if scope.visiting[name] {
		return nil
	}
	scope.visiting[name] = true
	defer delete(scope.visiting, name)
	return scope.helpers[name]
}

// argLabels resolves the labels a call passes to a helper through its parameters.
func argLabels(call *ast.CallExpr, scope *labelScope) []label {
	var keys []label
	for _, arg := range call.Args {
		keys = append(keys, keysInExpr(arg, scope)...)
	}
	return keys
}

// dedupeLabels drops repeats while keeping the first occurrence.
//
// The assignment map accumulates: `attrs = append(attrs, x)` records x twice, once through the
// identifier it reads and once through the value it appends, and a function with a dozen of those
// would otherwise report one label a few thousand times.
func dedupeLabels(labels []label) []label {
	seen := map[label]bool{}
	out := labels[:0]
	for _, l := range labels {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
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
	if !ok || len(call.Args) == 0 || !isAttributeConstructor(selector) {
		return "", false
	}
	return resolveKey(call.Args[0], consts)
}

// isAttributeConstructor reports whether the selector is one of the two attribute constructors this
// tree uses, in either spelling. It is the test that decides whether a call's first argument is a key
// position at all, which is what keeps the scan from reporting an unresolved key for every
// `attribute.Int(...)`-shaped call in a file.
func isAttributeConstructor(selector *ast.SelectorExpr) bool {
	switch selector.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Float64", "StringSlice":
	default:
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && (pkg.Name == "attribute" || pkg.Name == "telemetry")
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
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
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
