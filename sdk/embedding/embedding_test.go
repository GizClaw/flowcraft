package embedding

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// --- mock embedder ---

type mockEmbedder struct {
	vec []float32
	err error
	dim int
}

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return m.vec, m.err
}

func (m *mockEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = m.vec
	}
	return out, nil
}

func (m *mockEmbedder) Dimensions() int { return m.dim }

// --- EmbedText ---

func TestEmbedText_NilEmbedder(t *testing.T) {
	v, err := EmbedText(context.Background(), nil, "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != nil {
		t.Fatalf("expected nil, got %v", v)
	}
}

func TestEmbedText_Success(t *testing.T) {
	emb := &mockEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	v, err := EmbedText(context.Background(), emb, "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(v) != 3 || v[0] != 0.1 {
		t.Fatalf("got %v", v)
	}
}

func TestEmbedText_Error(t *testing.T) {
	emb := &mockEmbedder{err: errors.New("fail")}
	_, err := EmbedText(context.Background(), emb, "hello")
	if err == nil || err.Error() != "fail" {
		t.Fatalf("expected 'fail', got %v", err)
	}
}

// --- DimensionAware ---

func TestDimensionAware(t *testing.T) {
	emb := &mockEmbedder{dim: 768}
	da, ok := any(emb).(DimensionAware)
	if !ok {
		t.Fatal("expected DimensionAware")
	}
	if da.Dimensions() != 768 {
		t.Fatalf("Dimensions = %d, want 768", da.Dimensions())
	}
}

// --- ErrMissingCredentials ---

func TestErrMissingCredentials_IsUnauthorized(t *testing.T) {
	if !errdefs.IsUnauthorized(ErrMissingCredentials) {
		t.Fatal("ErrMissingCredentials should be classified as Unauthorized")
	}
}

func TestErrMissingCredentials_Message(t *testing.T) {
	if ErrMissingCredentials.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
}

// --- ProviderRegistry ---

func TestProviderRegistry_RegisterAndNewFromConfig(t *testing.T) {
	reg := NewProviderRegistry()
	factory := func(model string, config map[string]any) (Embedder, error) {
		return &mockEmbedder{vec: []float32{float32(len(model))}}, nil
	}
	reg.Register("test", factory)

	emb, err := reg.NewFromConfig("test", "my-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vec, _ := emb.Embed(context.Background(), "")
	if len(vec) != 1 || vec[0] != 8 {
		t.Fatalf("got %v", vec)
	}
}

func TestProviderRegistry_UnknownProvider(t *testing.T) {
	reg := NewProviderRegistry()
	_, err := reg.NewFromConfig("nonexistent", "model", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !errors.Is(err, err) {
		t.Fatal("error should exist")
	}
}

func TestProviderRegistry_FactoryError(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register("bad", func(string, map[string]any) (Embedder, error) {
		return nil, errors.New("factory fail")
	})
	_, err := reg.NewFromConfig("bad", "m", nil)
	if err == nil || err.Error() != "factory fail" {
		t.Fatalf("expected 'factory fail', got %v", err)
	}
}

func TestProviderRegistry_ListProviders(t *testing.T) {
	reg := NewProviderRegistry()
	if got := reg.ListProviders(); len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}

	reg.Register("beta", func(string, map[string]any) (Embedder, error) { return nil, nil })
	reg.Register("alpha", func(string, map[string]any) (Embedder, error) { return nil, nil })

	list := reg.ListProviders()
	if len(list) != 2 || list[0] != "alpha" || list[1] != "beta" {
		t.Fatalf("expected [alpha beta], got %v", list)
	}
}

func TestProviderRegistry_OverrideFactory(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register("p", func(string, map[string]any) (Embedder, error) {
		return &mockEmbedder{vec: []float32{1}}, nil
	})
	reg.Register("p", func(string, map[string]any) (Embedder, error) {
		return &mockEmbedder{vec: []float32{2}}, nil
	})

	emb, _ := reg.NewFromConfig("p", "m", nil)
	vec, _ := emb.Embed(context.Background(), "")
	if vec[0] != 2 {
		t.Fatalf("expected override to take effect, got %v", vec[0])
	}
}

func TestProviderRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewProviderRegistry()
	const n = 100
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			reg.Register("provider", func(string, map[string]any) (Embedder, error) {
				return &mockEmbedder{}, nil
			})
		}(i)
		go func(idx int) {
			defer wg.Done()
			reg.ListProviders()
			reg.NewFromConfig("provider", "m", nil) //nolint:errcheck
		}(i)
	}
	wg.Wait()
}

// --- DefaultRegistry / package-level functions ---

func TestDefaultRegistry_RegisterAndNewFromConfig(t *testing.T) {
	testProvider := "test_default_pkg_" + t.Name()
	RegisterProvider(testProvider, func(model string, config map[string]any) (Embedder, error) {
		return &mockEmbedder{vec: []float32{42}}, nil
	})

	emb, err := NewFromConfig(testProvider, "m", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vec, _ := emb.Embed(context.Background(), "")
	if vec[0] != 42 {
		t.Fatalf("got %v", vec[0])
	}

	list := ListProviders()
	found := false
	for _, p := range list {
		if p == testProvider {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%q not in ListProviders: %v", testProvider, list)
	}
}

func TestDefaultRegistry_UnknownProvider(t *testing.T) {
	_, err := NewFromConfig("__no_such_provider__", "m", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider via package-level func")
	}
}
