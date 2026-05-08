package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdkx/engine/checkpoint/conformance"
	"github.com/GizClaw/flowcraft/sdkx/engine/checkpoint/postgres"
)

// Each subtest gets its own table name so concurrent / repeated
// runs of `go test ./...` against the same database do not collide.
var tableSeq int64

func freshTable(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&tableSeq, 1)
	return fmt.Sprintf("ckpt_test_%d_%d", os.Getpid(), n)
}

func TestStoreContract(t *testing.T) {
	dsn := os.Getenv("FC_PG_DSN")
	if dsn == "" {
		t.Skip("FC_PG_DSN not set; skipping postgres integration tests")
	}

	conformance.RunSuite(t, func(t *testing.T) engine.CheckpointStore {
		table := freshTable(t)
		s, err := postgres.Open(context.Background(), dsn, postgres.WithTable(table))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() {
			// Best-effort: drop the table so repeated runs don't accumulate.
			_ = s.Close()
		})
		return s
	})
}

func TestMigrateIdempotent(t *testing.T) {
	dsn := os.Getenv("FC_PG_DSN")
	if dsn == "" {
		t.Skip("FC_PG_DSN not set")
	}
	s, err := postgres.Open(context.Background(), dsn, postgres.WithTable(freshTable(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Errorf("Migrate idempotency: %v", err)
	}
}
