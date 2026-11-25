package telemetry_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestInstrumentsDeclareTheirShape is the semantic-convention half of the metric rules:
// label cardinality is what internal/layout forbids, and this is what a metric owes the
// person reading it.
//
// Two requirements, both of them a one-line fix at the declaration and neither of them
// checkable at runtime — an instrument with no description renders as a bare name in
// every dashboard, and a duration histogram with no unit is a number whose axis nobody
// can label. A test that emits the instrument cannot see the difference: it asserted the
// value, and the value is right.
//
// Only the *declaration* is read, so this covers instruments no test happens to emit.
// That isdeliberate: the emitter is what a later refactor moves, and the declaration is
// what the operator's query depends on.
func TestInstrumentsDeclareTheirShape(t *testing.T) {
	// exempt is by "<dir>: <instrument name>", with the reason it cannot carry a unit.
	// Durations and byte counts always can, so this starts empty and should stay near it.
	exempt := map[string]string{}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	workspace := filepath.Dir(root)

	declarations := 0
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
		set := token.NewFileSet()
		file, parseErr := parser.ParseFile(set, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return relErr
		}
		dir := filepath.ToSlash(filepath.Dir(rel))

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			kind, isInstrument := instrumentKind(selector.Sel.Name)
			if !isInstrument || len(call.Args) < 2 {
				return true
			}
			name, ok := stringLiteral(call.Args[0])
			if !ok {
				// A computed name is not a declaration this can read; the emitter's own
				// test pins it.
				return true
			}
			declarations++
			where := set.Position(call.Pos())
			key := dir + ": " + name
			if _, ok := exempt[key]; ok {
				return true
			}
			if description := optionArg(call.Args[1:], "WithDescription"); description == "" {
				t.Errorf("%s: %s %q declares no metric.WithDescription; every dashboard renders "+
					"the bare name and nobody knows what it counts", where, kind, name)
			}
			// A histogram is a distribution of a measured quantity, and a quantity
			// without a unit is unreadable: "duration" in seconds or milliseconds is the
			// difference between a p99 alert that fires and one that never does.
			if kind == "histogram" && optionArg(call.Args[1:], "WithUnit") == "" {
				t.Errorf("%s: histogram %q declares no metric.WithUnit; name the quantity "+
					"(\"s\", \"By\", \"{token}\")", where, name)
			}
			// The SDK's default boundaries are built for milliseconds. A seconds-valued
			// duration left on them reports everything above ten seconds as one bucket, so
			// the histogram is present, plausible and useless. A boundary set is a decision
			// about the data, and this is where it gets made.
			if kind == "histogram" && !hasOption(call.Args[1:], "WithExplicitBucketBoundaries") {
				t.Errorf("%s: histogram %q declares no metric.WithExplicitBucketBoundaries; "+
					"the default set is built for milliseconds, so a duration in seconds is "+
					"one bucket above ten (telemetry.DurationBoundsSeconds is the convention)",
					where, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan for instrument declarations: %v", err)
	}
	if declarations == 0 {
		t.Fatal("found no instrument declarations; the scan reads the constructor names, " +
			"so a rename of the metric API leaves this test passing on nothing")
	}
}

// instrumentKind maps a meter constructor to the kind it builds, and reports whether the
// call builds one at all.
func instrumentKind(sel string) (string, bool) {
	switch sel {
	case "Int64Counter", "Float64Counter":
		return "counter", true
	case "Int64UpDownCounter", "Float64UpDownCounter":
		return "updowncounter", true
	case "Int64Histogram", "Float64Histogram":
		return "histogram", true
	case "Int64Gauge", "Float64Gauge":
		return "gauge", true
	default:
		return "", false
	}
}

// optionArg returns the literal argument of the named metric option ("WithDescription",
// "WithUnit") among a declaration's options, or "" when the option is absent or its
// argument is not a literal.
func optionArg(args []ast.Expr, want string) string {
	for _, arg := range args {
		call, ok := arg.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != want {
			continue
		}
		value, ok := stringLiteral(call.Args[0])
		if ok {
			return value
		}
	}
	return ""
}

// hasOption reports whether the named option is among a declaration's options, whatever
// its argument.
func hasOption(args []ast.Expr, want string) bool {
	for _, arg := range args {
		call, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == want {
			return true
		}
	}
	return false
}

// stringLiteral reads a string literal or a package-level constant reference as text.
func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}
