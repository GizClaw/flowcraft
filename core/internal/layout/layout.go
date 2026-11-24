// Package layout is the layer table of this module, and the scanner the guards in
// layout_test.go read the tree with.
//
// It exists because "a package imports only from the layers below it" is a rule no
// compiler checks and every reviewer has to hold in their head. The table is the rule
// made data: a package that is not in it fails the guard, so a new package cannot be
// added without deciding where it sits.
//
// The layer numbers, and what belongs at each, are the whole design of the module:
//
//	L0 vocabulary     values every layer can name: message, model, policy, identity, ...
//	L1 boundary       the two contracts that touch the outside world: sandbox, inference
//	L2 tool           the tool contract, its gate, its chain and its projection
//	L3 execution      one turn of an agent, and the ways turns compose
//	L4 assembly       the process: runtime, host, deploy
//
// internal/* sits at L0 in the table because every layer may use it; that is a claim
// about direction, not about importance, and it holds only as long as nothing under
// internal imports upward — which is what the guard checks.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Module is this module's import path.
const Module = "github.com/GizClaw/flowcraft/core"

// The layers, lowest first.
const (
	// L0 is vocabulary: a contract or value every layer may name.
	L0 = iota
	// L1 is a boundary contract: what crosses into a sandbox or a model provider.
	L1
	// L2 is the tool contract and its gate.
	L2
	// L3 is execution: one agent turn, and the ways turns compose.
	L3
	// L4 is assembly: the process that owns everything built from the documents.
	L4
)

// Layer names for the table, so the guard can print where a package belongs.
var layerNames = [...]string{"L0", "L1", "L2", "L3", "L4"}

// Name renders a layer number.
func Name(layer int) string {
	if layer < 0 || layer >= len(layerNames) {
		return fmt.Sprintf("L?%d", layer)
	}
	return layerNames[layer]
}

// Table maps a package directory, relative to the module root and slash-separated, to
// its layer. The module root is ".".
//
// Adding a package means adding a line here, which is the point: the decision is made
// once, in one place, and reviewed as a table entry rather than inferred from whatever
// imports the new package happens to have.
var Table = map[string]int{
	// L0 — vocabulary.
	".":                       L0, // the module root holds doc only
	"config":                  L0,
	"errdefs":                 L0,
	"fsx":                     L0,
	"identity":                L0,
	"identity/inject":         L0,
	"internal/decode":         L0,
	"internal/layout":         L0,
	"internal/paths":          L0,
	"internal/pathtest":       L0,
	"internal/ptr":            L0,
	"internal/search":         L0,
	"message":                 L0,
	"message/media":           L0,
	"model":                   L0,
	"outcome":                 L0,
	"policy":                  L0,
	"skill":                   L0,
	"telemetry":               L0,
	"telemetry/setup":         L0,
	"telemetry/telemetrytest": L0,

	// L1 — boundary contracts.
	"sandbox":                 L1,
	"sandbox/sandboxtest":     L1,
	"inference":               L1,
	"inference/inferencetest": L1,
	"inference/route":         L1,

	// L3 — execution: one turn of an agent, above the tool contract it drives.
	"agent":           L3,
	"agent/agenttest": L3,

	// L4 — assembly: what turns documents into values, and the host that runs them.
	"deploy":         L4,
	"deploy/sources": L4,
	"host":           L4,

	// L2 — the tool contract and its gate.
	"tool":            L2,
	"tool/dispatch":   L2,
	"tool/platform":   L2,
	"tool/projection": L2,
	"tool/tooltest":   L2,

	// L4 — assembly. Empty for the same reason: runtime, host and deploy land with
	// their entries.
}

// BannedNames are directory names that say nothing about what a package does.
var BannedNames = []string{
	"util", "utils", "common", "shared", "base", "core", "types", "interfaces",
	"contracts", "engine", "impl", "helper", "helpers", "misc",
}

// Package is one Go package as the guards see it.
type Package struct {
	// Dir is the directory relative to the module root, slash-separated ("." for the
	// root).
	Dir string
	// Imports are the core packages imported by the package's non-test files.
	Imports []string
	// TestImports are the core packages imported only by its test files.
	TestImports []string
	// External are the non-core import paths of either kind, which is what catches an
	// import of a sibling module.
	External []string
}

// Layer reports the layer of dir, and whether the table knows it.
//
// It resolves by longest prefix so the table does not have to repeat a parent's answer:
// an entry for "tool" covers "tool/x" only if "tool/x" is not itself listed, which the
// guards forbid.
func Layer(dir string) (int, bool) {
	layer, ok := Table[dir]
	return layer, ok
}

// Scan walks the module and returns every package, sorted by directory.
//
// Test files are separated so a guard can say which rule applies to which: a test may
// legitimately import a fixture from a higher layer (tooltest is L2 and every tool family
// test uses it), while production code may not.
func Scan() ([]Package, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	byDir := map[string]*Package{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		pkgImports, err := importsOf(path)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir = filepath.ToSlash(dir)
		pkg, ok := byDir[dir]
		if !ok {
			pkg = &Package{Dir: dir}
			byDir[dir] = pkg
		}
		production := !strings.HasSuffix(path, "_test.go")
		for _, imp := range pkgImports {
			if rest, isCore := strings.CutPrefix(imp, Module); isCore {
				rest = strings.TrimPrefix(rest, "/")
				if rest == "" {
					rest = "."
				}
				if production {
					pkg.Imports = appendUnique(pkg.Imports, rest)
				} else {
					pkg.TestImports = appendUnique(pkg.TestImports, rest)
				}
				continue
			}
			pkg.External = appendUnique(pkg.External, imp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Package, 0, len(byDir))
	for _, pkg := range byDir {
		sort.Strings(pkg.Imports)
		sort.Strings(pkg.TestImports)
		sort.Strings(pkg.External)
		out = append(out, *pkg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// Root returns the module root: the nearest ancestor of the working directory that holds
// a go.mod naming [Module].
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.Contains(string(data), "module "+Module) {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("layout: no go.mod for %s above the working directory", Module)
		}
		dir = parent
	}
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}
