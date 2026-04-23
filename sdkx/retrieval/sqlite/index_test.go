package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/contract"

	sqlx "github.com/GizClaw/flowcraft/sdkx/retrieval/sqlite"
)

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) (retrieval.Index, func()) {
		dir := t.TempDir()
		idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
		if err != nil {
			t.Fatal(err)
		}
		return idx, func() {}
	})
}

func TestDrop(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(t.Context(), "ns_drop", []retrieval.Doc{{ID: "x", Content: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Drop(t.Context(), "ns_drop"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(t.Context(), "ns_drop", "x"); ok {
		t.Fatal("expected ns dropped")
	}
}
