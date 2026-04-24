package history

import (
	"context"
	"github.com/GizClaw/flowcraft/sdk/recall"
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

func TestAllCategories(t *testing.T) {
	cats := recall.AllCategories()
	if len(cats) < 6 {
		t.Fatalf("expected at least 6 categories, got %d", len(cats))
	}
	expected := map[recall.Category]bool{
		recall.CategoryProfile: true, recall.CategoryPreferences: true, recall.CategoryEntities: true,
		recall.CategoryEvents: true, recall.CategoryCases: true, recall.CategoryPatterns: true,
	}
	for _, c := range expected {
		_ = c
	}
	for _, want := range []recall.Category{recall.CategoryProfile, recall.CategoryPreferences, recall.CategoryEntities, recall.CategoryEvents, recall.CategoryCases, recall.CategoryPatterns} {
		found := false
		for _, c := range cats {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing built-in category %q", want)
		}
	}
}

func TestRegisterCategory(t *testing.T) {
	recall.RegisterCategory("custom_test_cat")
	cats := recall.AllCategories()
	found := false
	for _, c := range cats {
		if c == "custom_test_cat" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected custom_test_cat in recall.AllCategories after recall.RegisterCategory")
	}

	// Duplicate registration should be idempotent.
	before := len(recall.AllCategories())
	recall.RegisterCategory("custom_test_cat")
	after := len(recall.AllCategories())
	if after != before {
		t.Fatalf("duplicate recall.RegisterCategory changed count: %d -> %d", before, after)
	}
}

func TestAllCategoryStrings(t *testing.T) {
	strs := recall.AllCategoryStrings()
	if len(strs) < 6 {
		t.Fatalf("expected at least 6 category strings, got %d", len(strs))
	}
	found := false
	for _, s := range strs {
		if s == "profile" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'profile' in recall.AllCategoryStrings")
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
