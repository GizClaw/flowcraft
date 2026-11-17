package runtime

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRuntimeDoesNotImportABackend guards the seam this package is.
//
// The runtime selects a backend by the name a deployment writes in
// sandbox.backend, from the table the host registers. Importing a backend here
// would undo that: every consumer of the runtime would link bwrap, seatbelt,
// windows, remote and the pty stack behind them, whether or not it ships one of
// them. The backends' constructors are unchanged and stay importable — the table
// a host builds lives with the host (core/runtime/backends is the convenience
// set, and it imports this package, not the other way round).
func TestRuntimeDoesNotImportABackend(t *testing.T) {
	const backendPrefix = "github.com/GizClaw/flowcraft/core/sandbox-"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed++
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if strings.HasPrefix(path, backendPrefix) {
				t.Errorf("%s imports %q: a backend belongs in the host's table, not in the composition root",
					name, path)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no files: the guard would pass for the wrong reason")
	}
}
