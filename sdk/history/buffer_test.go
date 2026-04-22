package history

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestBufferMemory_Load(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	msgs := make([]model.Message, 10)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, "c1", msgs)

	buf := NewBufferMemory(store, 3)
	loaded, err := buf.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3, got %d", len(loaded))
	}
}

func TestBufferMemory_DefaultMaxMessages(t *testing.T) {
	buf := NewBufferMemory(NewInMemoryStore(), 0)
	if buf.maxMessages != 50 {
		t.Fatalf("expected 50, got %d", buf.maxMessages)
	}
}

func TestBufferMemory_SaveAndClear(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	buf := NewBufferMemory(store, 10)

	_ = buf.Save(ctx, "c1", []model.Message{model.NewTextMessage(model.RoleUser, "hi")})
	loaded, _ := buf.Load(ctx, "c1")
	if len(loaded) != 1 {
		t.Fatal("expected 1 message")
	}

	_ = buf.Clear(ctx, "c1")
	loaded, _ = buf.Load(ctx, "c1")
	if len(loaded) != 0 {
		t.Fatal("expected 0 after clear")
	}
}
