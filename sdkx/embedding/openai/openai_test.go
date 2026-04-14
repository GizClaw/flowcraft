package openai

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/embedding"
)

func TestNew_EmptyKey(t *testing.T) {
	if e := New("", ""); e != nil {
		t.Fatal("expected nil for empty key")
	}
}

func TestNew_DefaultModel(t *testing.T) {
	e := New("test-key", "")
	if e == nil {
		t.Fatal("expected non-nil")
	}
	if e.model != defaultModel {
		t.Fatalf("model: got %q want %q", e.model, defaultModel)
	}
}

func TestNew_CustomModel(t *testing.T) {
	e := New("test-key", "text-embedding-3-large")
	if e.model != "text-embedding-3-large" {
		t.Fatalf("model: got %q", e.model)
	}
}

func TestImplementsInterface(t *testing.T) {
	var _ embedding.Embedder = (*Embedder)(nil)
}

func TestToFloat32(t *testing.T) {
	in := []float64{1.0, 2.5, -3.14}
	out := toFloat32(in)
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if float64(out[i])-in[i] > 0.001 {
			t.Fatalf("value mismatch at %d: %f vs %f", i, out[i], in[i])
		}
	}
}
