package history

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestBuffer_Load(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	msgs := make([]model.Message, 10)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, "c1", msgs)

	buf := NewBuffer(store, WithBufferMax(3))
	loaded, err := buf.Load(ctx, "c1", Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3, got %d", len(loaded))
	}
}

func TestBuffer_DefaultMaxMessages(t *testing.T) {
	buf := NewBuffer(NewInMemoryStore()).(*buffer)
	if buf.maxMessages != 50 {
		t.Fatalf("expected 50, got %d", buf.maxMessages)
	}
}

func TestBuffer_SaveAndClear(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	buf := NewBuffer(store, WithBufferMax(10)).(*buffer)

	_ = buf.Append(ctx, "c1", []model.Message{model.NewTextMessage(model.RoleUser, "hi")})
	loaded, _ := buf.Load(ctx, "c1", Budget{})
	if len(loaded) != 1 {
		t.Fatal("expected 1 message")
	}

	_ = buf.Clear(ctx, "c1")
	buf.mu.Lock()
	_, lockPresent := buf.locks["c1"]
	buf.mu.Unlock()
	if lockPresent {
		t.Fatal("expected Clear to remove per-conversation lock")
	}
	loaded, _ = buf.Load(ctx, "c1", Budget{})
	if len(loaded) != 0 {
		t.Fatal("expected 0 after clear")
	}
}
