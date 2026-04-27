package history

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestInMemoryStore_CRUD(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	convID := "conv-1"

	msgs, err := store.GetMessages(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty, got %d", len(msgs))
	}

	input := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi there"),
	}
	if err := store.SaveMessages(ctx, convID, input); err != nil {
		t.Fatal(err)
	}

	msgs, err = store.GetMessages(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2, got %d", len(msgs))
	}
	if msgs[0].Content() != "hello" {
		t.Fatalf("expected hello, got %q", msgs[0].Content())
	}

	if err := store.DeleteMessages(ctx, convID); err != nil {
		t.Fatal(err)
	}
	msgs, _ = store.GetMessages(ctx, convID)
	if len(msgs) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(msgs))
	}
}

func TestInMemoryStore_Isolation(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	_ = store.SaveMessages(ctx, "a", []model.Message{model.NewTextMessage(model.RoleUser, "msg-a")})
	_ = store.SaveMessages(ctx, "b", []model.Message{model.NewTextMessage(model.RoleUser, "msg-b")})

	msgsA, _ := store.GetMessages(ctx, "a")
	msgsB, _ := store.GetMessages(ctx, "b")

	if len(msgsA) != 1 || msgsA[0].Content() != "msg-a" {
		t.Fatal("isolation broken for conv a")
	}
	if len(msgsB) != 1 || msgsB[0].Content() != "msg-b" {
		t.Fatal("isolation broken for conv b")
	}
}

func TestInMemoryStore_ConcurrentGetMessages(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()
	ctx := context.Background()

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi"),
	}
	_ = store.SaveMessages(ctx, "conv", msgs)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.GetMessages(ctx, "conv")
			if err != nil {
				t.Errorf("GetMessages: %v", err)
				return
			}
			if len(got) != 2 {
				t.Errorf("expected 2, got %d", len(got))
			}
		}()
	}
	wg.Wait()
}

func TestBudget_IsZero(t *testing.T) {
	if !(Budget{}).IsZero() {
		t.Fatal("zero Budget should report IsZero")
	}
	if (Budget{MaxTokens: 1}).IsZero() {
		t.Fatal("MaxTokens=1 must not be zero")
	}
	if (Budget{MaxMessages: 1}).IsZero() {
		t.Fatal("MaxMessages=1 must not be zero")
	}
}
