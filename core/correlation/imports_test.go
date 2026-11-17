package correlation

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestImportsStayContractsTier guards the reason this package is top-level at
// all: it may depend on the standard library and the OpenTelemetry attribute
// package, and on nothing else.
//
// Both regressions it rules out are ones this package has already had. A
// telemetry import is how it ended up depending on the observability layer for
// seven strings, and any core/* import would make every layer below the assembly
// depend on that layer — the reason the carrier could not stay in
// core/runtime/runctx. A source parse is used rather than a hand-written list so
// the test cannot drift from the imports it guards.
func TestImportsStayContractsTier(t *testing.T) {
	allowed := map[string]bool{
		"context":                            true,
		"go.opentelemetry.io/otel/attribute": true,
	}
	for _, file := range []string{"doc.go", "run.go", "attrs.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q: core/correlation has to stay a contracts-tier leaf",
					file, path)
			}
		}
	}
}
