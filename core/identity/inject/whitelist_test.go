package inject_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyAllowedPackagesImportInject is the enforcement behind "three injection
// points", and the reason this package exists as a subpackage at all.
//
// Go cannot hide an exported function from one importer, so the rule is checked
// instead of expressed: no production package outside the allowlist may import
// core/identity/inject. Tests are exempt — a test has no other way to put a session
// on a context, and a test cannot mislabel a real trace.
//
// The allowlist is the plan's three injection points. Two of them do not exist yet,
// which is why they are listed anyway: arriving at one is then a no-op instead of an
// argument, and a package that reaches for this one has to say who it is.
func TestOnlyAllowedPackagesImportInject(t *testing.T) {
	const modulePrefix = "github.com/GizClaw/flowcraft/core/"
	allowed := map[string]bool{
		modulePrefix + "identity/inject": true, // the package itself
		modulePrefix + "host":            true, // OpenSession: the only layer that knows the bind
		modulePrefix + "agent":           true, // one turn: the only layer that knows the task
		modulePrefix + "delegation":      true, // a derived run: the only layer that knows the lineage
	}

	coreRoot := filepath.Join("..", "..")
	scanned := 0
	var offenders []string

	err := filepath.WalkDir(coreRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return skipDir(entry.Name(), path)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			if strings.Trim(spec.Path.Value, `"`) != modulePrefix+"identity/inject" {
				continue
			}
			// The package's own files are the definition, not a consumer.
			if strings.Contains(path, filepath.Join("identity", "inject")) {
				continue
			}
			if pkg := packagePath(coreRoot, path); !allowed[modulePrefix+pkg] {
				offenders = append(offenders, pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// There is no importer yet — the host and the agent do not exist — so the guard
	// asserts that the scan reached the module instead. A walk that silently stopped
	// would otherwise pass every rule below it.
	if scanned < 30 {
		t.Fatalf("scanned %d files, want the whole module", scanned)
	}
	for _, offender := range offenders {
		t.Errorf("%s imports identity/inject; only the three injection points may write identity", offender)
	}
}

// skipDir names the directories a repo-wide scan must not descend into: fixture
// trees are not compiled, and a ".test" suffix marks a directory of documents rather
// than Go code.
func skipDir(name, path string) error {
	if name == "testdata" || name == "vendor" || strings.HasSuffix(name, ".test") {
		return filepath.SkipDir
	}
	return nil
}

func packagePath(coreRoot, path string) string {
	rel, err := filepath.Rel(coreRoot, filepath.Dir(path))
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// TestIdentityAttachIsOnlyWrittenByInject is the same rule one level down: the
// carrier's writer is exported so this package can use it, which means anything could
// — and "anything" is exactly what the design forbids.
func TestIdentityAttachIsOnlyWrittenByInject(t *testing.T) {
	coreRoot := filepath.Join("..", "..")
	writers := 0

	err := filepath.WalkDir(coreRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return skipDir(entry.Name(), path)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(packagePath(coreRoot, path), "identity/") {
			return nil // the carrier and its writer
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Name != "identity" || selector.Sel.Name != "Attach" {
				return true
			}
			writers++
			t.Errorf("%s calls identity.Attach; identity is written through core/identity/inject only",
				packagePath(coreRoot, path))
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if writers != 0 {
		t.Fatalf("%d package(s) write identity directly", writers)
	}
}
