package layout_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/layout"
)

// The guards. Each one names a rule that no compiler checks, and each failure message
// says what to change rather than only what is wrong: these run in `make test`, so the
// message is read by whoever just broke it.

func packages(t *testing.T) []layout.Package {
	t.Helper()
	pkgs, err := layout.Scan()
	if err != nil {
		t.Fatalf("layout.Scan: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("layout.Scan found no packages")
	}
	return pkgs
}

// TestEveryPackageIsRegistered is the rule that keeps the rest meaningful: a package
// missing from the table has no layer, so "imports only from below" cannot be checked
// for it.
func TestEveryPackageIsRegistered(t *testing.T) {
	for _, pkg := range packages(t) {
		if _, ok := layout.Layer(pkg.Dir); !ok {
			t.Errorf("%s is not in layout.Table: add it with its layer (L0 vocabulary, "+
				"L1 boundary, L2 tool, L3 execution, L4 assembly)", pkg.Dir)
		}
	}
	present := map[string]bool{}
	for _, pkg := range packages(t) {
		present[pkg.Dir] = true
	}
	for dir := range layout.Table {
		if dir == "." || present[dir] {
			continue
		}
		t.Errorf("layout.Table lists %s, which is not a package in the tree: a stale entry "+
			"makes the layer of a name that no longer exists look governed", dir)
	}
}

// TestLayerNames guards the vocabulary of the layers themselves: a name that says nothing
// about what a package does is how a module turns into a junk drawer.
func TestLayerNames(t *testing.T) {
	for _, pkg := range packages(t) {
		base := pkg.Dir
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		for _, banned := range layout.BannedNames {
			if base == banned {
				t.Errorf("%s is named %q: name the unit, not its position in some other "+
					"package's hierarchy", pkg.Dir, banned)
			}
		}
	}
}

// TestImportsPointDown is L-LAYER: every import is at or below the importer's layer.
//
// Test files are held to the same rule. A fixture lives in the layer whose contract it
// exercises, so a test that imports upward is a test that is in the wrong package — and
// letting it pass is how the rule stops meaning anything.
func TestImportsPointDown(t *testing.T) {
	for _, pkg := range packages(t) {
		self, ok := layout.Layer(pkg.Dir)
		if !ok {
			continue // reported by TestEveryPackageIsRegistered
		}
		for _, imported := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
			other, known := layout.Layer(imported)
			if !known {
				t.Errorf("%s imports %s, which has no layer", pkg.Dir, imported)
				continue
			}
			if other > self {
				t.Errorf("%s (%s) imports %s (%s): an import may only point down",
					pkg.Dir, layout.Name(self), imported, layout.Name(other))
			}
		}
	}
}

// TestIdentityIsAVocabularyLeaf is L-IDENTITY: identity names run identity, which every
// layer above it needs, so it may not depend on any of them.
func TestIdentityIsAVocabularyLeaf(t *testing.T) {
	for _, pkg := range packages(t) {
		if pkg.Dir != "identity" && pkg.Dir != "identity/inject" {
			continue
		}
		if len(pkg.Imports) > 0 && pkg.Dir == "identity" {
			t.Errorf("identity imports %v: it is the bottom of the graph", pkg.Imports)
		}
		for _, imported := range pkg.Imports {
			if imported != "identity" {
				t.Errorf("%s imports %s: identity/inject may only extend identity", pkg.Dir, imported)
			}
		}
	}
}

// TestPolicyStaysSilent is L-POLICY: policy decides, it does not report. Refusals travel
// back as a typed value, and the layer that acts on them records the telemetry.
func TestPolicyStaysSilent(t *testing.T) {
	allowed := map[string]bool{
		"errdefs":        true,
		"model":          true,
		"internal/paths": true,
	}
	for _, pkg := range packages(t) {
		if pkg.Dir != "policy" {
			continue
		}
		for _, imported := range pkg.Imports {
			if !allowed[imported] {
				t.Errorf("policy imports %s: it may import only errdefs, model and internal/*",
					imported)
			}
			if imported == "telemetry" {
				t.Error("policy imports telemetry: refuse with a *policy.Denial and let the " +
					"layer that acts on it record the event")
			}
		}
	}
}

// TestFSXKnowsNoPolicy is L-FSX: fsx takes paths and roots. Making it read a grant would
// put permission inside the thing permission is applied to, and make it impossible to
// describe a tree without a session.
func TestFSXKnowsNoPolicy(t *testing.T) {
	banned := map[string]bool{"policy": true, "tool": true, "sandbox": true}
	for _, pkg := range packages(t) {
		if pkg.Dir != "fsx" {
			continue
		}
		for _, imported := range pkg.Imports {
			if banned[imported] {
				t.Errorf("fsx imports %s: it takes []string, not a permission", imported)
			}
		}
	}
}

// TestVocabularyDoesNotImportTelemetry is the second half of L-API: a vocabulary package
// is a value, and a value that instruments itself cannot be used to describe something
// before a tracer exists.
func TestVocabularyDoesNotImportTelemetry(t *testing.T) {
	for _, pkg := range packages(t) {
		switch pkg.Dir {
		case "policy", "identity", "identity/inject", "model", "fsx", "errdefs":
		default:
			continue
		}
		for _, imported := range pkg.Imports {
			if imported == "telemetry" || strings.HasPrefix(imported, "telemetry/") {
				t.Errorf("%s imports %s: L0 vocabulary is instrumented by its callers, not "+
					"by itself", pkg.Dir, imported)
			}
		}
	}
}

// TestTelemetrySetupIsASeam keeps the SDK and its exporters behind one package: everything
// else uses the API surface, which is what lets a library be tested without a collector.
func TestTelemetrySetupIsASeam(t *testing.T) {
	for _, pkg := range packages(t) {
		if pkg.Dir == "telemetry" || pkg.Dir == "telemetry/setup" {
			continue // the package itself wires the seam; its own tests import it
		}
		for _, imported := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
			if imported == "telemetry/setup" {
				t.Errorf("%s imports telemetry/setup: only telemetry and the examples install "+
					"an exporter", pkg.Dir)
			}
		}
	}
}

// TestInjectIsCalledFromTheAssembly is L-INJECT: identity travels in the context, which is
// exactly the ambient state this module otherwise refuses, so the set of packages allowed
// to attach it stays small and named.
func TestInjectIsCalledFromTheAssembly(t *testing.T) {
	allowed := map[string]bool{"host": true, "agent": true, "identity/inject": true}
	for _, pkg := range packages(t) {
		for _, imported := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
			if imported == "identity/inject" && !allowed[pkg.Dir] {
				t.Errorf("%s imports identity/inject: only host and agent may attach run "+
					"identity to a context", pkg.Dir)
			}
		}
	}
}

// TestOnlyTheHostHoldsTheRuntime is L-ASSEMBLY: "the assembly layer owns the runtime" is
// only true if nothing else can reach it, which is a property of the import graph.
func TestOnlyTheHostHoldsTheRuntime(t *testing.T) {
	allowed := map[string]bool{"host": true}
	for _, pkg := range packages(t) {
		for _, imported := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
			if imported == "runtime" && !allowed[pkg.Dir] {
				t.Errorf("%s imports runtime: only host may hold the assembled process", pkg.Dir)
			}
		}
	}
}

// TestOptionalImplementationsAreNotImported is L-OPTIONAL: the contract may not reach for
// an implementation. Protocol families, tool sources and sandbox backends are chosen by a
// deployment document resolved at the composition root, so nothing here may name one.
func TestOptionalImplementationsAreNotImported(t *testing.T) {
	forbidden := []string{
		"tool-builtin/",
		"tool-mcp",
	}
	for _, pkg := range packages(t) {
		for _, imported := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
			for _, prefix := range forbidden {
				if strings.HasPrefix(imported, prefix) {
					t.Errorf("%s imports %s: an implementation is selected by a deployment "+
						"document, not by the contract", pkg.Dir, imported)
				}
			}
		}
	}
}

// TestCoreDoesNotImportSiblingModules keeps the module graph pointing one way: sandbox and
// inference are built on core, so core may not be built on them. This is what makes the
// split a split rather than a cycle spread across go.mod files.
func TestCoreDoesNotImportSiblingModules(t *testing.T) {
	siblings := []string{
		"github.com/GizClaw/flowcraft/sandbox",
		"github.com/GizClaw/flowcraft/inference",
		"github.com/GizClaw/flowcraft/driver",
	}
	for _, pkg := range packages(t) {
		for _, imported := range pkg.External {
			for _, sibling := range siblings {
				if imported == sibling || strings.HasPrefix(imported, sibling+"/") {
					t.Errorf("%s imports %s: core is the bottom of the module graph", pkg.Dir, imported)
				}
			}
		}
	}
}

// TestToolNamesNoSubsystem is the rule the capability slots exist for: the tool contract
// describes what a tool declares, and the implementation behind a declaration arrives as a
// value in the pocket. A field of type *fsx.Tree would put a subsystem in the contract.
func TestToolNamesNoSubsystem(t *testing.T) {
	forbidden := map[string]bool{"fsx": true, "sandbox": true, "inference": true, "skill": true}
	for _, pkg := range packages(t) {
		if pkg.Dir != "tool" {
			continue
		}
		for _, imported := range pkg.Imports {
			if forbidden[imported] {
				t.Errorf("tool imports %s: a capability is a slot the owning package "+
					"declares, so the contract names no subsystem", imported)
			}
		}
	}
}
