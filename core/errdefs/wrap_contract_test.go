package errdefs_test

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

// TestNoErrorRebuiltFromMessage is the gate for the one wrap break errorlint
// cannot see.
//
// errorlint covers fmt.Errorf verbs, `==` comparisons and bare type
// assertions. It does not cover the shape that walks away from an error
// entirely — errors.New(err.Error()) — because that is an ordinary call to
// errors.New with a string argument. The classified constructors are the same
// blind spot from the other side: errdefs.Validationf and friends take a
// format string, so a %v where a %w belonged is invisible to the linter too
// (the package doc records that as a review convention).
//
// This test closes the first half from the source, which needs no type
// information: an errors.New whose argument is an expression ending in
// .Error() — bare, parenthesized, or as one operand of a concatenation — is
// rebuilding an error out of its message, and every errors.Is/As route
// through it is gone. The import is resolved per file, so an alias counts.
//
// Two shapes stay outside it, both for the same reason: proving them needs
// types or dataflow. errors.New(fmt.Sprint(err)) describes the error in an
// argument the parser cannot type, and `msg := err.Error(); errors.New(msg)`
// hands the message through a variable. Both are still review finds.
//
// It scans this module only — each module answers for its own code, so
// driver and backends have no gate of their own yet.
func TestNoErrorRebuiltFromMessage(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	var offenders []string
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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		found, parseErr := rebuiltFromMessage(path)
		if parseErr != nil {
			return parseErr
		}
		offenders = append(offenders, found...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module: %v", walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("errors.New(x.Error()) severs the error chain; wrap with %%w or "+
			"classify the same error instead:\n\t%s",
			strings.Join(offenders, "\n\t"))
	}
}

// rebuiltFromMessage reports the positions in one file where an error is
// reconstructed from its message.
func rebuiltFromMessage(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	names := errorsImportNames(file)
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isErrorsNew(call, names) || len(call.Args) != 1 {
			return true
		}
		if fromMessage(call.Args[0]) {
			found = append(found, fset.Position(call.Pos()).String())
		}
		return true
	})
	return found, nil
}

// errorsImportNames resolves the names this file binds the stdlib errors
// package to: the usual `errors`, any alias, and "" for a dot-import, which
// makes New a bare identifier.
func errorsImportNames(file *ast.File) map[string]struct{} {
	names := make(map[string]struct{}, 1)
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != "errors" {
			continue
		}
		switch {
		case spec.Name == nil:
			names["errors"] = struct{}{}
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

// isErrorsNew reports whether call is a call to the stdlib errors.New under
// one of the names the file imports it by.
func isErrorsNew(call *ast.CallExpr, names map[string]struct{}) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fun.Sel.Name != "New" {
			return false
		}
		pkg, ok := fun.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, imported := names[pkg.Name]
		return imported
	case *ast.Ident:
		if fun.Name != "New" {
			return false
		}
		_, dotted := names[""]
		return dotted
	}
	return false
}

// fromMessage reports whether expr renders an error back into a string. Only
// Error() qualifies, on its own or inside a concatenation — an error that is
// only described in the argument (fmt.Sprint(err)) is the same severing, but
// without types the gate cannot tell that argument from a plain value, so it
// stays out rather than guessing.
func fromMessage(expr ast.Expr) bool {
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return fromMessage(node.X)
	case *ast.BinaryExpr:
		return fromMessage(node.X) || fromMessage(node.Y)
	case *ast.CallExpr:
		selector, ok := node.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return selector.Sel.Name == "Error" && len(node.Args) == 0
	}
	return false
}

// TestRebuiltFromMessageDetects pins the gate itself: a scanner that quietly
// stopped matching (a renamed helper, a changed node shape) would leave the
// check above green on a clean tree, and green is what a broken gate looks
// like.
func TestRebuiltFromMessageDetects(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "direct",
			src:  "package p\n\nimport \"errors\"\n\nfunc f(err error) error { return errors.New(err.Error()) }\n",
			want: true,
		},
		{
			name: "aliased import",
			src:  "package p\n\nimport stderr \"errors\"\n\nfunc f(err error) error { return stderr.New(err.Error()) }\n",
			want: true,
		},
		{
			name: "dot import",
			src:  "package p\n\nimport . \"errors\"\n\nfunc f(err error) error { return New(err.Error()) }\n",
			want: true,
		},
		{
			name: "interpolated prefix",
			src:  "package p\n\nimport \"errors\"\n\nfunc f(err error) error { return errors.New(\"fetch: \" + err.Error()) }\n",
			want: true,
		},
		{
			name: "wrapped",
			src:  "package p\n\nimport \"fmt\"\n\nfunc f(err error) error { return fmt.Errorf(\"fetch: %w\", err) }\n",
			want: false,
		},
		{
			name: "classified",
			src:  "package p\n\nfunc f(err error) error { return Validationf(\"fetch: %w\", err) }\n",
			want: false,
		},
		{
			name: "ordinary string",
			src:  "package p\n\nimport \"errors\"\n\nfunc f(s string) error { return errors.New(s) }\n",
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "case.go")
			if err := os.WriteFile(path, []byte(test.src), 0o600); err != nil {
				t.Fatalf("write case: %v", err)
			}
			found, err := rebuiltFromMessage(path)
			if err != nil {
				t.Fatalf("rebuiltFromMessage: %v", err)
			}
			if got := len(found) > 0; got != test.want {
				t.Fatalf("detected = %v (%v), want %v", got, found, test.want)
			}
		})
	}
}
