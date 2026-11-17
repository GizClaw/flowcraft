package contextkeys_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registered is the allowlist of files that declare a context key, keyed by the
// path from the module root. Adding a value means adding a row to
// docs/guides/context.md and an entry here; the test fails either way round.
//
// It is a list of files rather than of keys because the keys are unexported
// names the gate cannot spell from another package, and because the file is what
// the guide cites: a row without a file and a file without a row are both the
// drift this test exists to catch.
var registered = []string{
	"correlation/run.go",
	"inference/request_metadata.go",
	"inference/route/target.go",
	"tool/call_context.go",
	"tool/failure_kind.go",
	"tool/scope.go",
}

// TestEveryContextValueIsRegistered is the drift guard for the context registry.
//
// A context value is invisible at the seam: one layer writes it, five layers
// below read it, and nothing in a signature says so. The guide is therefore the
// only place the set is visible, and a guide maintained by hand goes stale in
// two directions — a value added without a row, and a row left behind after the
// value moved. Both are failures here:
//
//  1. a context.WithValue call in a file the allowlist does not name;
//  2. an allowlisted file that no longer calls it, or no longer exists;
//  3. an allowlisted file that docs/guides/context.md does not mention.
//
// The walk and the parse are the reason the guard cannot drift itself: the calls
// are discovered from the source, so the only hand-written list is the allowlist
// of files, which every failure names.
func TestEveryContextValueIsRegistered(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	guidePath, err := filepath.Abs("../../../docs/guides/context.md")
	if err != nil {
		t.Fatalf("resolve guide path: %v", err)
	}

	allowed := make(map[string]struct{}, len(registered))
	for _, file := range registered {
		allowed[file] = struct{}{}
	}
	declared := make(map[string][]string, len(registered))
	walked := make(map[string]struct{})
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		walked[rel] = struct{}{}
		lines, parseErr := withValueLines(path)
		if parseErr != nil {
			return parseErr
		}
		if len(lines) > 0 {
			declared[rel] = lines
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module: %v", walkErr)
	}

	// 1. No value can appear without a row: an unregistered writer is how the
	// set grows silently, which is what this gate is here to stop.
	for file, lines := range declared {
		if _, ok := allowed[file]; ok {
			continue
		}
		t.Errorf("core/%s calls context.WithValue at %s but is not in the registry: "+
			"add the value to docs/guides/context.md and its file to the registered list in this test",
			file, strings.Join(lines, ", "))
	}

	// 2. No row can outlive its value: an allowlist entry with nothing behind it
	// is a guide row that describes how the code used to work.
	for _, file := range registered {
		if _, ok := walked[file]; !ok {
			t.Errorf("core/%s is registered but does not exist: remove its row from docs/guides/context.md and its entry here", file)
			continue
		}
		if len(declared[file]) == 0 {
			t.Errorf("core/%s is registered but no longer calls context.WithValue: "+
				"remove its row from docs/guides/context.md and its entry here", file)
		}
	}

	// 3. The guide and the gate cannot disagree: the table has to name the file
	// its row is about, in the same spelling the walk produces.
	guide, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("read docs/guides/context.md: %v", err)
	}
	text := string(guide)
	for _, file := range registered {
		if !strings.Contains(text, "core/"+file) {
			t.Errorf("core/%s is registered here but docs/guides/context.md does not mention it: "+
				"the table and the gate have drifted apart", file)
		}
	}
}

// withValueLines returns the positions of every context.WithValue call in one
// file. The import is resolved per file, so an aliased context package is caught
// too.
func withValueLines(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	names := contextImportNames(file)
	var lines []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isWithValue(call, names) {
			return true
		}
		lines = append(lines, fset.Position(call.Pos()).String())
		return true
	})
	return lines, nil
}

// contextImportNames resolves the names this file binds the stdlib context
// package to: the usual `context`, any alias, and "" for a dot-import, which
// makes WithValue a bare identifier.
func contextImportNames(file *ast.File) map[string]struct{} {
	names := make(map[string]struct{}, 1)
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != "context" {
			continue
		}
		switch {
		case spec.Name == nil:
			names["context"] = struct{}{}
		case spec.Name.Name == "_":
			// Blank import: nothing is callable through it.
		case spec.Name.Name == ".":
			names[""] = struct{}{}
		default:
			names[spec.Name.Name] = struct{}{}
		}
	}
	return names
}

// isWithValue reports whether call is a call to context.WithValue under one of
// the names the file imports the package by.
func isWithValue(call *ast.CallExpr, names map[string]struct{}) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fun.Sel.Name != "WithValue" {
			return false
		}
		pkg, ok := fun.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, imported := names[pkg.Name]
		return imported
	case *ast.Ident:
		if fun.Name != "WithValue" {
			return false
		}
		_, dotted := names[""]
		return dotted
	}
	return false
}
