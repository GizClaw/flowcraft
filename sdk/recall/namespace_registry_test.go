package recall_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

func TestMemoryNamespaceRegistryListSortedDistinct(t *testing.T) {
	ctx := context.Background()
	reg := recall.NewMemoryNamespaceRegistry()
	if err := reg.Remember(ctx, "z_ns"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remember(ctx, "a_ns"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remember(ctx, "z_ns"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remember(ctx, ""); err != nil {
		t.Fatal(err)
	}

	got, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a_ns" || got[1] != "z_ns" {
		t.Fatalf("List() = %v, want [a_ns z_ns]", got)
	}
}
