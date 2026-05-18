package pipeline

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

type countingEmbedder struct {
	calls atomic.Int64
}

type slowEmbedder struct {
	calls atomic.Int64
	ready chan struct{}
}

func (s *slowEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if s.calls.Add(1) == 1 {
		close(s.ready)
	}
	time.Sleep(25 * time.Millisecond)
	return []float32{1, 0, 0}, nil
}

func (s *slowEmbedder) EmbedBatch(ctx context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		v, err := s.Embed(ctx, in[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
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

func TestEmbedQuerySingleflight(t *testing.T) {
	emb := &slowEmbedder{ready: make(chan struct{})}
	st := &EmbedQuery{Embedder: emb, MaxEntries: 16}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state := &State{Request: &retrieval.SearchRequest{QueryText: "same"}}
			errs <- st.Run(context.Background(), state)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := emb.calls.Load(); got != 1 {
		t.Fatalf("expected one embed call for concurrent identical queries, got %d", got)
	}
}
