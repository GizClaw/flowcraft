package craft

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// declarationTables are the charter tables themselves: they name
// permissions as data instead of gating on them. The audit skips their
// literals — the tables are cross-checked by TestCharterMatchesManifest,
// TestCharterMatchesStandard and TestPermissionCharterCoverage — but it
// still fails when a gate appears inside one, so nothing hides behind
// the skip.
var declarationTables = []string{
	filepath.Join("hostmcp", "charter.go"),
	filepath.Join("plugin", "charter.go"),
	filepath.Join("plugin", "manifest.go"),
}

// charterProbes names the runtime tests that pin one charter row's drop
// rule: the section is declared without its permission and must not
// surface. Every unexempted row names at least one, and the audit fails
// when a named test is gone, so a row cannot ship without a probe.
var charterProbes = map[string][]string{
	"hooks":  {"TestContributionsRequirePermission"},
	"mcp":    {"TestMCPSectionRequiresPermission", "TestPluginNodeWithoutMCPGrant"},
	"nodes":  {"TestPluginNodesRequirePermission"},
	"skills": {"TestContributionsRequirePermission"},
}

// charterScan is the parsed picture of the module's non-test sources.
type charterScan struct {
	// gated maps a permission to the sites of its gates, the negated
	// HasPermission checks that read it.
	gated map[string][]string
	// tableGates holds gates found in a declaration table, which is
	// never allowed.
	tableGates []string
	// dynamic holds HasPermission calls whose argument is not a bare
	// literal: the audit cannot tell which permission they cover.
	dynamic []string
	// grants maps a Grant field literal to its sites.
	grants map[string][]string
	// strays holds permission literals that are neither gates nor grant
	// declarations.
	strays []string
	// tests maps every test function name to its file.
	tests map[string]string
}

// scanCharterSources parses every Go file of the module below the
// current package, skipping testdata, hidden directories and generated
// test files.
func scanCharterSources(t *testing.T) *charterScan {
	t.Helper()
	tables := make(map[string]bool, len(declarationTables))
	for _, path := range declarationTables {
		path = filepath.ToSlash(path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("declaration table: %v", err)
		}
		tables[path] = true
	}
	permissions := make(map[string]bool)
	for _, permission := range plugin.ValidPermissions() {
		permissions[permission] = true
	}
	scan := &charterScan{
		gated:  map[string][]string{},
		grants: map[string][]string{},
		tests:  map[string]string{},
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == "." {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		if strings.HasSuffix(rel, "_test.go") {
			for _, decl := range file.Decls {
				function, ok := decl.(*ast.FuncDecl)
				if ok && function.Name != nil {
					scan.tests[function.Name.Name] = rel
				}
			}
			return nil
		}
		scan.file(fset, rel, file, permissions, tables[rel])
		return nil
	})
	if err != nil {
		t.Fatalf("scan module sources: %v", err)
	}
	return scan
}

// file records the role one parsed source file gives to permissions:
// gate, grant declaration, or none — the last one being a finding.
func (s *charterScan) file(
	fset *token.FileSet,
	path string,
	tree *ast.File,
	permissions map[string]bool,
	table bool,
) {
	at := func(pos token.Pos) string {
		return fmt.Sprintf("%s:%d", path, fset.Position(pos).Line)
	}
	gates := map[token.Pos]string{}
	allowed := map[token.Pos]bool{}
	ast.Inspect(tree, func(node ast.Node) bool {
		unary, ok := node.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			return true
		}
		call, ok := unary.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		if literal, ok := permissionLiteral(call); ok {
			gates[call.Pos()] = literal
			allowed[call.Args[0].Pos()] = true
		}
		return true
	})
	ast.Inspect(tree, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isPermissionCall(call) {
			return true
		}
		if _, ok := permissionLiteral(call); !ok {
			s.dynamic = append(s.dynamic, at(call.Pos()))
			return true
		}
		if table {
			s.tableGates = append(s.tableGates, at(call.Pos()))
			return true
		}
		if literal, isGate := gates[call.Pos()]; isGate {
			s.gated[literal] = append(s.gated[literal], at(call.Pos()))
		}
		return true
	})
	ast.Inspect(tree, func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok || key.Name != "Grant" {
			return true
		}
		value, ok := field.Value.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(value.Value)
		if err != nil {
			return true
		}
		allowed[value.Pos()] = true
		if !table {
			s.grants[text] = append(s.grants[text], at(field.Pos()))
		}
		return true
	})
	if table {
		return
	}
	ast.Inspect(tree, func(node ast.Node) bool {
		value, ok := node.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(value.Value)
		if err != nil || !permissions[text] || allowed[value.Pos()] {
			return true
		}
		s.strays = append(s.strays, fmt.Sprintf(
			"%s: permission %q is neither gated nor declared",
			at(value.Pos()), text))
		return true
	})
}

// isPermissionCall reports whether call is a HasPermission call.
func isPermissionCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "HasPermission" && len(call.Args) == 1
}

// permissionLiteral returns the permission a HasPermission call names;
// ok is false when the argument is not a bare string literal.
func permissionLiteral(call *ast.CallExpr) (string, bool) {
	if !isPermissionCall(call) {
		return "", false
	}
	value, ok := call.Args[0].(*ast.BasicLit)
	if !ok || value.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(value.Value)
	if err != nil {
		return "", false
	}
	return text, true
}

// TestCharterSourceGates audits the module's non-test sources against
// the contribution charter: an unexempted row needs its gate, an
// exempted row must not have one, and a permission may appear nowhere
// but in a gate, a grant declaration or a declaration table.
func TestCharterSourceGates(t *testing.T) {
	t.Parallel()
	scan := scanCharterSources(t)
	chartered := map[string]string{}
	for _, row := range plugin.Charter {
		chartered[row.Permission] = row.Kind
	}
	for _, row := range plugin.Charter {
		gates := scan.gated[row.Permission]
		switch {
		case row.Exemption == "" && len(gates) == 0:
			t.Errorf("charter %s: no !HasPermission(%q) gate in the "+
				"non-test sources", row.Kind, row.Permission)
		case row.Exemption != "" && len(gates) > 0:
			t.Errorf("charter %s: exempt (%s) yet gated at %v",
				row.Kind, row.Exemption, gates)
		}
	}
	for permission, sites := range scan.gated {
		if _, ok := chartered[permission]; !ok {
			t.Errorf("gate on %q at %v has no charter row", permission, sites)
		}
	}
	for _, site := range scan.dynamic {
		t.Errorf("permission read at %s is not a literal: the audit "+
			"cannot map it to a row", site)
	}
	for _, site := range scan.tableGates {
		t.Errorf("declaration table holds a gate at %s", site)
	}
	for _, stray := range scan.strays {
		t.Errorf("unregistered permission use: %s", stray)
	}
}

// TestCharterProbeCoverage requires every unexempted row to name the
// runtime tests that pin its drop rule, and every named test to exist.
func TestCharterProbeCoverage(t *testing.T) {
	t.Parallel()
	scan := scanCharterSources(t)
	for _, row := range plugin.Charter {
		probes := charterProbes[row.Kind]
		if row.Exemption != "" {
			if len(probes) > 0 {
				t.Errorf("charter %s: exempt row names probes %v",
					row.Kind, probes)
			}
			continue
		}
		if len(probes) == 0 {
			t.Errorf("charter %s: no probe pins its drop rule", row.Kind)
			continue
		}
		for _, probe := range probes {
			if _, ok := scan.tests[probe]; !ok {
				t.Errorf("charter %s: probe %s is not a test in this module",
					row.Kind, probe)
			}
		}
	}
	kinds := map[string]bool{}
	for _, row := range plugin.Charter {
		kinds[row.Kind] = true
	}
	for kind := range charterProbes {
		if !kinds[kind] {
			t.Errorf("charterProbes names unknown charter row %q", kind)
		}
	}
}

// TestPrimitiveGrantsChartered audits the primitive half of the table:
// the Grant literals of the registered tools and the hostmcp.Charter
// grants must be the same set, so a tool cannot ship a grant no row
// declares and a row cannot outlive its tool.
func TestPrimitiveGrantsChartered(t *testing.T) {
	t.Parallel()
	scan := scanCharterSources(t)
	declared := map[string]bool{}
	for _, row := range hostmcp.Charter {
		if row.Grant != "" {
			declared[row.Grant] = true
		}
	}
	for grant, sites := range scan.grants {
		if !declared[grant] {
			t.Errorf("registered grant %q at %v has no hostmcp.Charter row",
				grant, sites)
		}
	}
	for grant := range declared {
		if len(scan.grants[grant]) == 0 {
			t.Errorf("hostmcp.Charter grant %q is registered nowhere", grant)
		}
	}
}
