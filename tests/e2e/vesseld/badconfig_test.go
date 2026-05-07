package vesseld_e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/loader"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

// TestBadConfig_MissingEngineRef asserts an Agent that references
// an engine kind not registered in the catalog is rejected at
// resolve time. Pinning the missing kind name in the error is the
// "snapshot": a future refactor of the catalog error path that
// drops the kind name will show up here as a regression.
func TestBadConfig_MissingEngineRef(t *testing.T) {
	t.Parallel()
	objs, err := loader.Load(
		[]string{absBadcfg(t, "missing-engine-ref")},
		loader.Options{Recursive: true},
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, errs := resolver.Resolve(objs, catalog.Builtin(), resolver.ResolveOptions{})
	if errs.Len() == 0 {
		t.Fatal("expected resolve errors for missing-engine-ref fixture, got none")
	}
	got := errs.Error()
	if !strings.Contains(got, "this-engine-kind-does-not-exist") {
		t.Errorf("error did not mention the missing engine kind;\nfull message:\n%s", got)
	}
}

// TestBadConfig_DuplicateVessel asserts the same Vessel name
// declared twice is rejected. Loaders that silently win-on-last
// would cause confusing routing surprises later; rejecting at
// load/resolve time is the user-friendly behaviour.
func TestBadConfig_DuplicateVessel(t *testing.T) {
	t.Parallel()
	objs, err := loader.Load(
		[]string{absBadcfg(t, "duplicate-vessel")},
		loader.Options{Recursive: true},
	)
	if err != nil {
		// loader rejected at parse time — that is also acceptable.
		if !strings.Contains(err.Error(), "alpha") {
			t.Fatalf("loader error did not mention duplicate name 'alpha': %v", err)
		}
		return
	}
	_, errs := resolver.Resolve(objs, catalog.Builtin(), resolver.ResolveOptions{})
	if errs.Len() == 0 {
		t.Fatal("expected resolver errors for duplicate-vessel fixture")
	}
}

func absBadcfg(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "badconfig", name))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return p
}
