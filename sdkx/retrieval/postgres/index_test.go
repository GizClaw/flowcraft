package postgres_test

import (
	"context"
	"os"
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
