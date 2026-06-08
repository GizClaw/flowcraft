package claw

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
)

const (
	defaultEmbeddingCacheTTL = 5 * time.Minute
	defaultEmbeddingCacheMax = 1024
)

type cachedEmbedder struct {
	inner embedding.Embedder
	ttl   time.Duration
	max   int

	mu       sync.Mutex
	cache    map[string]embeddingCacheEntry
	inflight map[string]*embeddingInflight
}

type embeddingCacheEntry struct {
	vec []float32
	exp time.Time
}

type embeddingInflight struct {
	done chan struct{}
	vec  []float32
	err  error
}

func newCachedEmbedder(inner embedding.Embedder) embedding.Embedder {
	if inner == nil {
		return nil
	}
	return &cachedEmbedder{
		inner: inner,
		ttl:   defaultEmbeddingCacheTTL,
		max:   defaultEmbeddingCacheMax,
	}
}

func (e *cachedEmbedder) Dimensions() int {
	if d, ok := e.inner.(embedding.DimensionAware); ok {
		return d.Dimensions()
	}
	return 0
}

func (e *cachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	key := strings.TrimSpace(text)
	if key == "" {
		return e.inner.Embed(ctx, text)
	}
	if e.ttl <= 0 || e.max <= 0 {
		return e.inner.Embed(ctx, text)
	}

	e.mu.Lock()
	if e.cache == nil {
		e.cache = make(map[string]embeddingCacheEntry)
	}
	if entry, ok := e.cache[key]; ok {
		if time.Now().Before(entry.exp) {
			vec := append([]float32(nil), entry.vec...)
			e.mu.Unlock()
			return vec, nil
		}
		delete(e.cache, key)
	}
	if e.inflight == nil {
		e.inflight = make(map[string]*embeddingInflight)
	}
	if call, ok := e.inflight[key]; ok {
		e.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			vec := append([]float32(nil), call.vec...)
			return vec, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &embeddingInflight{done: make(chan struct{})}
	e.inflight[key] = call
	e.mu.Unlock()

	vec, err := e.inner.Embed(ctx, text)

	e.mu.Lock()
	if err == nil {
		e.storeLocked(key, vec)
	}
	call.vec = append([]float32(nil), vec...)
	call.err = err
	delete(e.inflight, key)
	close(call.done)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return vec, nil
}

func (e *cachedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	missingTexts := make([]string, 0, len(texts))
	missingIndexes := make([]int, 0, len(texts))

	now := time.Now()
	e.mu.Lock()
	if e.cache == nil {
		e.cache = make(map[string]embeddingCacheEntry)
	}
	for i, text := range texts {
		key := strings.TrimSpace(text)
		if key == "" {
			missingTexts = append(missingTexts, text)
			missingIndexes = append(missingIndexes, i)
			continue
		}
		if entry, ok := e.cache[key]; ok && now.Before(entry.exp) {
			out[i] = append([]float32(nil), entry.vec...)
			continue
		}
		if entry, ok := e.cache[key]; ok && !now.Before(entry.exp) {
			delete(e.cache, key)
		}
		missingTexts = append(missingTexts, text)
		missingIndexes = append(missingIndexes, i)
	}
	e.mu.Unlock()

	if len(missingTexts) == 0 {
		return out, nil
	}
	vecs, err := e.inner.EmbedBatch(ctx, missingTexts)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	for i, vec := range vecs {
		if i >= len(missingIndexes) {
			break
		}
		idx := missingIndexes[i]
		out[idx] = vec
		key := strings.TrimSpace(texts[idx])
		if key != "" {
			e.storeLocked(key, vec)
		}
	}
	e.mu.Unlock()
	return out, nil
}

func (e *cachedEmbedder) storeLocked(key string, vec []float32) {
	if e.cache == nil {
		e.cache = make(map[string]embeddingCacheEntry)
	}
	if len(e.cache) >= e.max {
		now := time.Now()
		for k, entry := range e.cache {
			if !now.Before(entry.exp) {
				delete(e.cache, k)
				break
			}
		}
	}
	if len(e.cache) >= e.max {
		for k := range e.cache {
			delete(e.cache, k)
			break
		}
	}
	e.cache[key] = embeddingCacheEntry{
		vec: append([]float32(nil), vec...),
		exp: time.Now().Add(e.ttl),
	}
}
