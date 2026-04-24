package pipeline

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

type countingEmbedder struct {
	calls atomic.Int64
}

func (c *countingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	c.calls.Add(1)
	return []float32{1, 0, 0}, nil
}

func (c *countingEmbedder) EmbedBatch(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		c.calls.Add(1)
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func TestEmbedQueryCacheRespectsMaxEntries(t *testing.T) {
	emb := &countingEmbedder{}
	st := &EmbedQuery{Embedder: emb, MaxEntries: 4}
	for i := 0; i < 32; i++ {
		state := &State{Request: &retrieval.SearchRequest{QueryText: fmt.Sprintf("q%02d", i)}}
		if err := st.Run(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	size := len(st.cache)
	st.mu.Unlock()
	if size > 4 {
		t.Fatalf("cache exceeded MaxEntries=4: size=%d", size)
	}
}

func TestEmbedQueryCacheDisabled(t *testing.T) {
	emb := &countingEmbedder{}
	st := &EmbedQuery{Embedder: emb, MaxEntries: -1}
	for i := 0; i < 3; i++ {
		state := &State{Request: &retrieval.SearchRequest{QueryText: "same"}}
		if err := st.Run(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	if got := emb.calls.Load(); got != 3 {
		t.Fatalf("expected 3 embed calls (cache off), got %d", got)
	}
	if st.cache != nil && len(st.cache) > 0 {
		t.Fatalf("cache should be empty when disabled, got %+v", st.cache)
	}
}
