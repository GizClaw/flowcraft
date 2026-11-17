package config

import (
	"context"
	"testing"

	"github.com/goccy/go-json"
)

// FuzzLoad: a layer is untrusted input — a file from a workspace, a document a
// host fetched. Whatever it holds, loading must not panic, and a stack that
// loads must canonicalize to valid JSON: every later stage reads those bytes.
func FuzzLoad(f *testing.F) {
	for _, seed := range [][2]string{
		{"version: 1\nname: forge\n", "name: local\n"},
		{"version: 1\nextra: {a: 1}\n", "extra: {a: 2}\n"},
		{"version: 1\n---\nname: second\n", ""},
		{"# only comments\n", "version: 1\n"},
		{`{"version":1,"name":"json"}`, ""},
		{"version: 1\nextra: {a: ${MISSING}}\n", ""},
		{"version: 1\n~: 1\n", ""},
		{"", ""},
		{"{}\n", "[]\n"},
		{"\xff\xfe", "version: 1\n"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, base, override string) {
		layers := []Layer{
			{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(base))},
			{Priority: 10, Name: "local.yaml", Source: Bytes([]byte(override))},
		}
		_, doc, err := Load[map[string]any](context.Background(), layers, Strict[map[string]any])
		if err != nil {
			return
		}
		canonical, err := doc.Canonical()
		if err != nil {
			return
		}
		if !json.Valid(canonical) {
			t.Fatalf("Canonical returned invalid JSON for base=%q override=%q: %q",
				base, override, canonical)
		}
		// The invariant the guard exists for: whatever path Canonical took, it
		// must render what the exact path renders. A YAML library upgrade that
		// changed either path's resolution fails here instead of in a deployment.
		if exact, exactErr := doc.canonicalViaYAML(); exactErr == nil && string(exact) != string(canonical) {
			t.Fatalf("Canonical disagreed with the exact path for base=%q override=%q:\n canonical=%s\n exact=%s",
				base, override, canonical, exact)
		}
	})
}
