package layout_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/layout"
)

// These three guards cover what TestImportsPointDown cannot: the import graph is a
// compile-time fact, and each of the rules below is a fact about *when* something runs.
// Package-level init, a second place that installs a process-wide provider, and a test
// that shares a package with the instrumentation kit are all invisible to the compiler
// and to the layer table, and all three are how a rule survives review and dies in
// production.

// coreFile is one parsed file, with the two facts the guards below need about where it
// lives.
type coreFile struct {
	// path is the file's path relative to the module root, slash-separated.
	path string
	// dir is the package directory relative to the module root ("." for the root).
	dir string
	// test reports whether the file is a _test.go file.
	test bool
	// file is the parsed syntax tree.
	file *ast.File
}

// coreFiles parses every Go file of this module. It is the walk the guards share; each
// guard only inspects the subset it governs.
func coreFiles(t *testing.T) []coreFile {
	t.Helper()
	root, err := layout.Root()
	if err != nil {
		t.Fatalf("layout.Root: %v", err)
	}
	var files []coreFile
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		files = append(files, coreFile{
			path: filepath.ToSlash(rel),
			dir:  filepath.ToSlash(filepath.Dir(rel)),
			test: strings.HasSuffix(rel, "_test.go"),
			file: file,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatal("scanned no Go files; the guards below would pass vacuously")
	}
	return files
}

// TestNoPackageLevelInit is the assembly half of the layer rule: package-level init runs
// before any host decision — before a plan is resolved, before a grant is narrowed, before
// a provider is installed — so it is the one place a package can acquire a dependency, an
// order, or a side effect that no table and no reader can see.
//
// The allowlist is empty on purpose. The one entry this module had (a no-op store into an
// atomic.Value whose reader already tolerated nil) was dead code, and the guard is what
// found it: the fix was deleting it, not listing it.
func TestNoPackageLevelInit(t *testing.T) {
	allowed := map[string]string{}
	for _, file := range coreFiles(t) {
		if file.test {
			continue
		}
		for _, decl := range file.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "init" {
				continue
			}
			if why, ok := allowed[file.dir]; ok {
				t.Logf("%s: init allowed (%s)", file.path, why)
				continue
			}
			t.Errorf("%s: package-level init; it runs before any host decision, so a "+
				"dependency or an ordering it establishes is invisible to the layer table. "+
				"Move it to an explicit constructor the composition root calls — or delete "+
				"it, if (as here) nothing needs it", file.path)
		}
	}
}

// TestOnlyTheSetupInstallsAProvider keeps the observability seam single. The OpenTelemetry
// tracer, meter, logger and propagator providers are process-global: a second install site
// silently reroutes every span, metric and log line for the whole process, which is a bug
// the instrumentation tests cannot see because each of them installs its own.
//
// The rule is about production files. A test that installs a provider is how
// instrumentation is verified at all (see TestAPackageUsingTheProviderKitRunsItsTestsSerially
// for the constraint that comes with it).
func TestOnlyTheSetupInstallsAProvider(t *testing.T) {
	// installs a process-wide provider, by the name of the call.
	installs := map[string]bool{
		"SetTracerProvider":    true,
		"SetMeterProvider":     true,
		"SetLoggerProvider":    true,
		"SetTextMapPropagator": true,
	}
	// A directory is allowed by its place in the module, not by its name: the two entries
	// are the production seam and the kit that stands in for it in tests.
	allowed := map[string]string{
		"telemetry/setup":         "the seam: InitTrace/InitMeter/InitLog install what a deployment asked for",
		"telemetry/telemetrytest": "the kit: it is the test-time seam, and it ships as a non-test package so other modules can install it",
	}
	for _, file := range coreFiles(t) {
		if file.test {
			continue
		}
		ast.Inspect(file.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !installs[selector.Sel.Name] {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "otel" {
				return true
			}
			if why, ok := allowed[file.dir]; ok {
				t.Logf("%s: %s allowed (%s)", file.path, selector.Sel.Name, why)
				return true
			}
			t.Errorf("%s: otel.%s from %s installs process-wide state outside the setup seam; "+
				"every span, metric and log line in the process is routed by whichever call ran last",
				file.path, selector.Sel.Name, file.dir)
			return true
		})
	}
}

// TestAPackageUsingTheProviderKitRunsItsTestsSerially is the constraint the kit cannot
// enforce on itself. telemetrytest swaps a process-global provider and holds a lock for
// the test's lifetime, so a parallel test in the *same package* — same process, same
// provider — puts its spans in the other test's recorder, and the failure reads as a
// flake in whichever test happened to run second.
//
// Packages are separate processes, so parallelism between packages is fine; the check is
// per directory for that reason. Adding t.Parallel next to the kit is refused here rather
// than discovered in a nightly run.
func TestAPackageUsingTheProviderKitRunsItsTestsSerially(t *testing.T) {
	// kitDirs is every package whose tests install a provider through the kit.
	kitDirs := map[string]string{}
	for _, file := range coreFiles(t) {
		uses := false
		ast.Inspect(file.file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "New" && selector.Sel.Name != "Meters" {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "telemetrytest" {
				uses = true
				return false
			}
			return true
		})
		if uses {
			kitDirs[file.dir] = file.path
		}
	}
	if len(kitDirs) == 0 {
		t.Fatal("found no package using telemetrytest; the guard would pass vacuously")
	}
	for _, file := range coreFiles(t) {
		if !file.test {
			continue
		}
		first, ok := kitDirs[file.dir]
		if !ok {
			continue
		}
		ast.Inspect(file.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Parallel" {
				return true
			}
			if recv, ok := selector.X.(*ast.Ident); !ok || recv.Name != "t" {
				return true
			}
			t.Errorf("%s: t.Parallel in a package that uses the telemetry kit (%s); "+
				"the kit's provider is process-wide, so the two tests would share one "+
				"recorder. Drop t.Parallel, or move the assertion into its own package",
				file.path, first)
			return true
		})
	}
}
