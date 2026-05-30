package history_test

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/history"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// TestInMemoryStore_AppendMessages_NoLostBatchesUnderRace is the
// regression guard for issue #154: pre-fix, InMemoryStore lacked a
// MessageAppender implementation, so the RMW caller pattern lost
// batches on every concurrent same-conversation Save. Run with
// `-race` to also catch any residual data races in the
// MessageAppender implementation.
func TestInMemoryStore_AppendMessages_NoLostBatchesUnderRace(t *testing.T) {
	store := history.NewInMemoryStore()
	defer store.Close()

	const (
		workers     = 16
		appendsEach = 32
	)
	conv := "conv-shared"
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < appendsEach; i++ {
				msg := model.Message{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "x"}}}
				if err := store.AppendMessages(context.Background(), conv, []model.Message{msg}); err != nil {
					t.Errorf("AppendMessages: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	got, err := store.GetMessages(context.Background(), conv)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	want := workers * appendsEach
	if len(got) != want {
		t.Fatalf("lost batches under concurrent Append: got %d messages, want %d (#154 regression)", len(got), want)
	}
}

// TestInMemoryStore_ImplementsMessageAppender pins the capability
// assertion: history.appendHistory and compactor.persistAppend
// both probe for this interface and pick the atomic path when it
// is present. If a future refactor accidentally drops the
// implementation, every default-config deployment regresses to
// the RMW fallback (#154 / #162).
func TestInMemoryStore_ImplementsMessageAppender(t *testing.T) {
	var s any = history.NewInMemoryStore()
	if _, ok := s.(history.MessageAppender); !ok {
		t.Fatalf("InMemoryStore must implement history.MessageAppender (#154)")
	}
}
