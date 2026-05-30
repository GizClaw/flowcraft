package recall

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

// TestExpireRetired_HardDeletesExpiredFacts pins that ExpireRetired
// hard-deletes facts whose ExpiresAt is in the past relative to the
// supplied cutoff and leaves the rest intact.
func TestExpireRetired_HardDeletesExpiredFacts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	for i, exp := range []*time.Time{&past, &past, &past, &future, &future} {
		fact := TemporalFact{
			Kind:      FactNote,
			Content:   "f" + string(rune('a'+i)),
			ExpiresAt: exp,
		}
		if _, err := mem.Save(context.Background(), scope, SaveRequest{Facts: []TemporalFact{fact}}); err != nil {
			t.Fatalf("seed save %d: %v", i, err)
		}
	}

	deleted, err := mem.ExpireRetired(context.Background(), scope, now)
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}
	got, _ := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if len(got) != 2 {
		t.Errorf("surviving facts = %d, want 2", len(got))
	}
}

// TestExpireRetired_NoExpiredFactsReturnsZero pins the empty-sweep
// contract: a clean scope returns (0, nil) without touching the
// store or projections.
func TestExpireRetired_NoExpiredFactsReturnsZero(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := mem.ExpireRetired(context.Background(), scope, time.Now())
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}
