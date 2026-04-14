package memory

import (
	"context"
	"testing"
)

func TestNewWithLLM_NilLLMDegradesToBuffer(t *testing.T) {
	mem, err := NewWithLLM(Config{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.(*BufferMemory); !ok {
		t.Fatal("expected BufferMemory fallback when LLM is nil")
	}
}

func TestNewWithLLM_DeprecatedTypeStillWorks(t *testing.T) {
	for _, typ := range []string{"buffer", "window", "summary", "token"} {
		mem, err := NewWithLLM(Config{Type: typ}, nil, nil, nil)
		if err != nil {
			t.Fatalf("type %q: %v", typ, err)
		}
		if _, ok := mem.(*BufferMemory); !ok {
			t.Fatalf("type %q: expected BufferMemory fallback when LLM is nil", typ)
		}
	}
}

func TestNewWithLLM_WithLongTermStore(t *testing.T) {
	ltStore := &mockLongTermStore{}
	mem, err := NewWithLLM(Config{
		LongTerm: LongTermConfig{Enabled: true},
	}, nil, nil, ltStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.(*MemoryAwareMemory); !ok {
		t.Fatal("expected MemoryAwareMemory wrapper when ltStore is provided")
	}
}

func TestNewWithLLM_NilWorkspaceDegradesToBuffer(t *testing.T) {
	ml := &mockSummaryLLM{}
	// LLM provided but no workspace → should fall back to buffer.
	mem, err := NewWithLLM(Config{}, nil, ml, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.(*BufferMemory); !ok {
		t.Fatal("expected BufferMemory fallback when workspace is nil")
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	if cfg.maxMessages() != 50 {
		t.Fatalf("expected 50, got %d", cfg.maxMessages())
	}
}

type mockLongTermStore struct{}

func (m *mockLongTermStore) Save(context.Context, string, *MemoryEntry) error { return nil }
func (m *mockLongTermStore) List(context.Context, string, ListOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (m *mockLongTermStore) Search(context.Context, string, string, SearchOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (m *mockLongTermStore) Update(context.Context, string, *MemoryEntry) error { return nil }
func (m *mockLongTermStore) Delete(context.Context, string, string) error       { return nil }
