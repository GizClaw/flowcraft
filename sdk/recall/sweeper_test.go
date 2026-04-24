package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestSweepOnceDeletesAcrossRememberedNamespaces(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	now := time.Now()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithClock(func() time.Time { return now }),
		recall.WithNamespaceRegistry(recall.NewMemoryNamespaceRegistry()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scopeA := newScope()
	scopeB := scopeA
	scopeB.UserID = "u2"
	past := now.Add(-time.Second)
	idA, err := m.Add(ctx, scopeA, recall.Entry{Content: "expired A", ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := m.Add(ctx, scopeB, recall.Entry{Content: "expired B", ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}

	sweeper, ok := m.(interface {
		SweepOnce(context.Context) error
	})
	if !ok {
		t.Fatal("Memory does not expose SweepOnce")
	}
	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := idx.Get(ctx, recall.NamespaceFor(scopeA), idA); ok {
		t.Fatalf("expired doc %q in namespace A was not deleted", idA)
	}
	if _, ok, _ := idx.Get(ctx, recall.NamespaceFor(scopeB), idB); ok {
		t.Fatalf("expired doc %q in namespace B was not deleted", idB)
	}
}
