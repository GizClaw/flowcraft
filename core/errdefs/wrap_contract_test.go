package errdefs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// .Error() is rebuilding an error out of its message, and every
// errors.Is/As route through it is gone. It scans this module only — each
// module answers for its own code.
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
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isErrorsNew(call) || len(call.Args) != 1 {
			return true
		}
		if fromMessage(call.Args[0]) {
			found = append(found, fset.Position(call.Pos()).String())
		}
		return true
	})
	return found, nil
}

// isErrorsNew reports whether call is a call to the stdlib errors.New.
func isErrorsNew(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "New" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "errors"
}

// fromMessage reports whether expr renders an error back into a string. Only
// Error() qualifies: fmt.Sprint takes any value, and a panic value formatted
// into a string is not a chain to preserve.
func fromMessage(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return selector.Sel.Name == "Error" && len(call.Args) == 0
}
