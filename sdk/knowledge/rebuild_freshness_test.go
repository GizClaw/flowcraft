package knowledge_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
)

// countingEmbedder wraps stubEmbedder with a per-call counter so
// tests can assert exactly how many embed operations Rebuild
// triggered.
type countingEmbedder struct {
	inner stubEmbedder
	calls int64
}

func (e *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	atomic.AddInt64(&e.calls, 1)
	return e.inner.Embed(ctx, text)
}

func (e *countingEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	atomic.AddInt64(&e.calls, int64(len(texts)))
	return e.inner.EmbedBatch(ctx, texts)
}

// TestService_RebuildSkipsFreshDocsWhenChunkSigReaderAvailable pins
// the freshness gate that fixed #152: when the configured
// ChunkRepo implements [knowledge.ChunkSigReader] (the retrieval-
// backed RetrievalChunkRepo does), a second Rebuild against an
// already-up-to-date corpus must NOT re-spend the embedding quota.
//
// Pre-fix Rebuild unconditionally re-chunked + re-embedded every
// doc in scope on every call, wasting embedder budget on a
// stable corpus.
func TestService_RebuildSkipsFreshDocsWhenChunkSigReaderAvailable(t *testing.T) {
	emb := &countingEmbedder{inner: stubEmbedder{dim: 4}}
	svc := newService(t, factory.WithRetrievalEmbedder(emb, "test-sig"))
	ctx := context.Background()

	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("PutDocument: %v", err)
	}
	firstEmbedCalls := atomic.LoadInt64(&emb.calls)
	if firstEmbedCalls == 0 {
		t.Fatalf("#152: setup precondition — initial PutDocument should have driven the embedder at least once; got 0")
	}

	// Rebuild a freshly-ingested doc: sig matches → embedder must
	// remain idle.
	if err := svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds"}); err != nil {
		t.Fatalf("Rebuild (fresh): %v", err)
	}
	if got := atomic.LoadInt64(&emb.calls); got != firstEmbedCalls {
		t.Fatalf("#152: Rebuild on fresh doc must NOT re-embed; calls went %d -> %d", firstEmbedCalls, got)
	}

	// PutDocument again with new content bumps SourceVer →
	// next Rebuild MUST re-embed.
	if err := svc.PutDocument(ctx, "ds", "a.md", "beta gamma delta"); err != nil {
		t.Fatalf("PutDocument v2: %v", err)
	}
	afterV2 := atomic.LoadInt64(&emb.calls)
	if afterV2 <= firstEmbedCalls {
		t.Fatalf("#152: PutDocument v2 must drive embedder; calls %d -> %d", firstEmbedCalls, afterV2)
	}
	if err := svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds"}); err != nil {
		t.Fatalf("Rebuild (fresh again): %v", err)
	}
	if got := atomic.LoadInt64(&emb.calls); got != afterV2 {
		t.Fatalf("#152: Rebuild on still-fresh v2 must NOT re-embed; calls %d -> %d", afterV2, got)
	}
}
