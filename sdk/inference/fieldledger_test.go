package inference

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// TestFieldIDConstsAreReachable guards the reverse ledger invariant: no
// FieldID const may be orphaned. A const is reachable when its value is used
// by a ledger tag on a struct field or its name is referenced from package
// code (including tests and subpackages). TestRequestFieldsDeclareLedgerMetadata
// covers the forward direction (every tag must be a known const).
func TestFieldIDConstsAreReachable(t *testing.T) {
	fileSet := token.NewFileSet()
	consts := map[string]string{} // name -> value

	declFile, err := parser.ParseFile(fileSet, "decision.go", nil, 0)
	if err != nil {
		t.Fatalf("parse decision.go: %v", err)
	}
	ast.Inspect(declFile, func(node ast.Node) bool {
		gen, ok := node.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
				continue
			}
			typeName := ""
			if ident, ok := valueSpec.Type.(*ast.Ident); ok {
				typeName = ident.Name
			}
			if typeName != "FieldID" {
				continue
			}
			lit, ok := valueSpec.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", valueSpec.Names[0].Name, err)
			}
			consts[valueSpec.Names[0].Name] = value
		}
		return true
	})
	if len(consts) == 0 {
		t.Fatal("no FieldID consts found in decision.go")
	}

	tags := map[string]bool{}
	identifiers := map[string]bool{}
	err = filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || path == "decision.go" {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				identifiers[n.Name] = true
			case *ast.SelectorExpr:
				identifiers[n.Sel.Name] = true
			case *ast.StructType:
				for _, field := range n.Fields.List {
					if field.Tag == nil {
						continue
					}
					raw := field.Tag.Value
					tag := reflect.StructTag(raw[1 : len(raw)-1]).Get("ledger")
					if tag != "" {
						tags[tag] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk package: %v", err)
	}

	for name, value := range consts {
		if tags[value] || identifiers[name] {
			continue
		}
		t.Errorf("FieldID const %s (%q) is not referenced by any ledger tag or package code", name, value)
	}
}
