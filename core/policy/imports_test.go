package policy_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolicyImportsOnlyItsAllowlist is L-POLICY, enforced before the repo-wide
// layouter exists.
//
// The rule is what keeps the vocabulary usable from every layer: a policy that
// imported telemetry would drag the OTel SDK into the one package that every
// layer below the assembly needs, and a policy that imported the tool or sandbox
// contracts would turn the permission value into an upward dependency.
func TestPolicyImportsOnlyItsAllowlist(t *testing.T) {
	const modulePrefix = "github.com/GizClaw/flowcraft/core/"
	allowed := map[string]bool{
		modulePrefix + "errdefs": true,
		modulePrefix + "model":   true,
	}
	isInternal := func(path string) bool {
		return strings.HasPrefix(path, modulePrefix+"internal/")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		scanned++
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, spec := range parsed.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !strings.HasPrefix(path, modulePrefix) {
				continue // standard library or an external module
			}
			if allowed[path] || isInternal(path) {
				continue
			}
			t.Errorf("%s imports %q, which is outside the policy allowlist", file, path)
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d files, want the whole package — the glob is not reaching it", scanned)
	}
}

// TestPolicyDoesNotImportTelemetry is the specific half of the rule that a
// reviewer is most likely to undo: refusals are returned as values, so no part of
// this package may record them.
func TestPolicyDoesNotImportTelemetry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, spec := range parsed.Imports {
			if strings.Contains(spec.Path.Value, "/telemetry") || strings.Contains(spec.Path.Value, "opentelemetry") {
				t.Errorf("%s imports %s; refusals are values, not spans", file, spec.Path.Value)
			}
		}
	}
}
