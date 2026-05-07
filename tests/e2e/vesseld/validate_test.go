package vesseld_e2e

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/loader"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

// TestMultiVesselFixture_StillValid is the schema-drift gate. It
// runs every time `make test` runs (no build tag) and ensures the
// multi-vessel fixture under testdata/multi-vessel cannot silently
// rot when we evolve the v1alpha1 schema or the resolver.
//
// The fixture is intentionally close to a real production layout
// (one Daemon doc, shared/ for LLMProfile, vessels/<name>/ for
// per-vessel Vessel + Agent docs, dispatcher + worker + standalone
// vessel patterns) so a schema break in any of those shapes shows
// up here.
//
// We drive the fixture via the same loader+resolver that
// `vesseld validate` uses, in IO-free mode (AllowFile=false,
// AllowSecret=false), so the test never depends on env vars or
// external files.
func TestMultiVesselFixture_StillValid(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("testdata/multi-vessel")
	if err != nil {
		t.Fatal(err)
	}
	objs, err := loader.Load([]string{root}, loader.Options{Recursive: true})
	if err != nil {
		t.Fatalf("load %s: %v", root, err)
	}
	if len(objs) == 0 {
		t.Fatalf("loader returned 0 objects from %s — fixture folder gone?", root)
	}
	_, errs := resolver.Resolve(objs, catalog.Builtin(), resolver.ResolveOptions{})
	if errs.Len() > 0 {
		t.Fatalf("resolver rejected the multi-vessel fixture:\n%s", errs.Error())
	}
}
