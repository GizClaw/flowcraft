package history

import "testing"

func TestNewWithLLM_NilLLMDegradesToBuffer(t *testing.T) {
	mem, err := NewWithLLM(Config{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.(*BufferMemory); !ok {
		t.Fatal("expected BufferMemory fallback when LLM is nil")
	}
}

func TestNewWithLLM_DeprecatedTypeStillWorks(t *testing.T) {
	for _, typ := range []string{"buffer", "window", "summary", "token"} {
		mem, err := NewWithLLM(Config{Type: typ}, nil, nil)
		if err != nil {
			t.Fatalf("type %q: %v", typ, err)
		}
		if _, ok := mem.(*BufferMemory); !ok {
			t.Fatalf("type %q: expected BufferMemory fallback when LLM is nil", typ)
		}
	}
}

func TestNewWithLLM_NilWorkspaceDegradesToBuffer(t *testing.T) {
	ml := &mockSummaryLLM{}
	// LLM provided but no workspace → should fall back to buffer.
	mem, err := NewWithLLM(Config{}, nil, ml)
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
