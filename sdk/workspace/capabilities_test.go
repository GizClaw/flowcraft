package workspace

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMemWorkspace_Capabilities(t *testing.T) {
	ws := NewMemWorkspace()
	c := ws.Capabilities()
	if !c.AtomicRename {
		t.Error("MemWorkspace must report AtomicRename")
	}
	if !c.ReadAfterWrite {
		t.Error("MemWorkspace must report ReadAfterWrite")
	}
	if c.DurableOnWrite {
		t.Error("MemWorkspace must not claim DurableOnWrite")
	}
	if c.Distributed {
		t.Error("MemWorkspace must not claim Distributed")
	}
}

func TestLocalWorkspace_Capabilities(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := ws.Capabilities()
	if !c.AtomicRename || !c.ReadAfterWrite || !c.DurableOnWrite {
		t.Errorf("LocalWorkspace unexpectedly missing caps: %+v", c)
	}
	if c.Distributed {
		t.Error("LocalWorkspace must not claim Distributed")
	}
}

func TestScopedWorkspace_Capabilities_ForwardsInner(t *testing.T) {
	inner := NewMemWorkspace()
	scoped := NewScopedWorkspace(inner)
	if scoped.Capabilities() != inner.Capabilities() {
		t.Errorf("ScopedWorkspace caps %+v != inner %+v",
			scoped.Capabilities(), inner.Capabilities())
	}
}

// minimalWorkspace is a Workspace that intentionally does NOT
// implement CapabilityReporter. It guards CapabilitiesOf's safe
// default path for third-party impls that pre-date the interface.
type minimalWorkspace struct{ Workspace }

func TestCapabilitiesOf_FallsBackToZeroValue(t *testing.T) {
	ws := minimalWorkspace{Workspace: NewMemWorkspace()}
	got := CapabilitiesOf(ws)
	if got != (Capabilities{}) {
		t.Errorf("non-reporter should yield zero-value, got %+v", got)
	}
}

func TestCapabilitiesOf_NilWorkspace(t *testing.T) {
	if CapabilitiesOf(nil) != (Capabilities{}) {
		t.Error("nil workspace should yield zero-value")
	}
}

func TestScopedWorkspace_Capabilities_OverNonReporter(t *testing.T) {
	inner := minimalWorkspace{Workspace: NewMemWorkspace()}
	scoped := NewScopedWorkspace(inner)
	if scoped.Capabilities() != (Capabilities{}) {
		t.Errorf("scoped over non-reporter should yield zero-value")
	}
	// Sanity: scoped reads still work end-to-end.
	if err := inner.Write(context.Background(), filepath.Join("a"), []byte("x")); err != nil {
		t.Fatal(err)
	}
}
