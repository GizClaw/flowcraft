package tooltest

import (
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

// CatalogFactory builds a fresh tool.Catalog for each subtest.
type CatalogFactory func() tool.Catalog

// CatalogSuite runs the contract every tool.Catalog implementation should
// pass.
func CatalogSuite(t *testing.T, f CatalogFactory) {
	t.Helper()
	t.Run("DefinitionsSorted", func(t *testing.T) { catalogDefinitionsSorted(t, f) })
	t.Run("GetMatchesDefinitions", func(t *testing.T) { catalogGetMatchesDefinitions(t, f) })
	t.Run("MissingToolAbsent", func(t *testing.T) { catalogMissingToolAbsent(t, f) })
}

func catalogDefinitionsSorted(t *testing.T, f CatalogFactory) {
	t.Helper()
	catalog := f()
	if catalog == nil {
		t.Fatal("factory returned nil catalog")
	}
	definitions := catalog.Definitions()
	if !sort.SliceIsSorted(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	}) {
		t.Fatalf("Definitions() not sorted: %+v", definitions)
	}
}

func catalogGetMatchesDefinitions(t *testing.T, f CatalogFactory) {
	t.Helper()
	catalog := f()
	for _, definition := range catalog.Definitions() {
		got, ok := catalog.Get(definition.Name)
		if !ok {
			t.Fatalf("Get(%q) missing from catalog", definition.Name)
		}
		if got == nil {
			t.Fatalf("Get(%q) returned nil tool", definition.Name)
		}
		if got.Definition().Name != definition.Name {
			t.Fatalf("Get(%q) definition name = %q", definition.Name, got.Definition().Name)
		}
	}
}

func catalogMissingToolAbsent(t *testing.T, f CatalogFactory) {
	t.Helper()
	catalog := f()
	if got, ok := catalog.Get("tooltest-missing-tool"); ok || got != nil {
		t.Fatalf("Get(missing) = (%v, %v), want (nil, false)", got, ok)
	}
}
