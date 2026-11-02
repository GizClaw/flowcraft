package projection

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// OtherNames reports what one family holds state under for one scope,
// excluding the name asked about: it is how a lane that reads an empty name
// learns that the state an earlier producer wrote is still on disk.
func TestOtherNamesReportsTheNamesAFamilyHoldsStateUnder(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store := newIdentityStore(t, kvStore, "lane-v1", 1)
	scope := corememory.Scope{RuntimeID: "runtime"}
	otherScope := corememory.Scope{RuntimeID: "other-runtime"}
	for _, name := range []string{"facts", "facts.vector-1a2b"} {
		if err := store.FullRebuild(ctx, testAddress(scope), name, map[string]bool{"base": true}, "source"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FullRebuild(ctx, testAddress(otherScope), "facts", map[string]bool{"base": true}, "source"); err != nil {
		t.Fatal(err)
	}
	names, err := store.OtherNames(ctx, scope, "facts.vector-1a2b")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"facts"}) {
		t.Fatalf("other names = %v, want [facts]", names)
	}
	names, err = store.OtherNames(ctx, scope, "facts")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"facts.vector-1a2b"}) {
		t.Fatalf("other names = %v, want [facts.vector-1a2b]", names)
	}
	// The fork case asks about a name nothing was ever written under: the
	// family's names still come back, which is the whole report.
	names, err = store.OtherNames(ctx, scope, "facts.vector-never-written")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"facts", "facts.vector-1a2b"}) {
		t.Fatalf("other names = %v, want both names of the family", names)
	}
	// Another scope of the same family has one name, the one asked about, so
	// it has nothing to report.
	names, err = store.OtherNames(ctx, otherScope, "facts")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("other names = %v, want none", names)
	}
}
