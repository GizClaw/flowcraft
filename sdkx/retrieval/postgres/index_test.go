package postgres_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/contract"

	pgx "github.com/GizClaw/flowcraft/sdkx/retrieval/postgres"
)

// TestContract requires FC_PG_DSN to be set, e.g.:
//
//	FC_PG_DSN=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
//
// Otherwise the test is skipped (default in CI without a Postgres service).
func TestContract(t *testing.T) {
	dsn := os.Getenv("FC_PG_DSN")
	if dsn == "" {
		t.Skip("FC_PG_DSN not set; skipping postgres integration tests")
	}
	contract.Run(t, func(t *testing.T) (retrieval.Index, func()) {
		idx, err := pgx.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		return idx, func() {
			ctx := context.Background()
			for _, ns := range []string{"ns_a", "ns_idem", "ns_raw", "ns_x", "ns_y", "ns_list", "ns_filt", "ns_rng"} {
				_ = idx.Drop(ctx, ns)
			}
		}
	})
}

func TestSearchSelectiveFilterBeyondInitialWindow(t *testing.T) {
	dsn := os.Getenv("FC_PG_DSN")
	if dsn == "" {
		t.Skip("FC_PG_DSN not set; skipping postgres integration test")
	}
	ctx := context.Background()
	idx, err := pgx.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	const ns = "ns_selective_pg"
	_ = idx.Drop(ctx, ns)
	t.Cleanup(func() { _ = idx.Drop(context.Background(), ns) })
	docs := make([]retrieval.Doc, 0, 80)
	for i := 0; i < 79; i++ {
		docs = append(docs, retrieval.Doc{
			ID:       "common-" + strconv.Itoa(i),
			Content:  "alpha alpha alpha common",
			Metadata: map[string]any{"tenant": "common"},
		})
	}
	docs = append(docs, retrieval.Doc{
		ID:       "rare",
		Content:  "alpha rare",
		Metadata: map[string]any{"tenant": "rare"},
	})
	if err := idx.Upsert(ctx, ns, docs); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "alpha",
		TopK:      1,
		Filter:    retrieval.Filter{Eq: map[string]any{"tenant": "rare"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "rare" {
		t.Fatalf("hits = %+v, want rare", resp.Hits)
	}
}
