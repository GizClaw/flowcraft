//go:build integration

package embedding_test

import (
	"context"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdkx/internal/testenv"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
)

func init() {
	testenv.Load()
}

// Integration tests require real credentials. Set:
//
//	EMBEDDING_PROVIDER  (e.g. "azure" or "openai")
//	EMBEDDING_API_KEY
//	EMBEDDING_MODEL     (e.g. "text-embedding-3-large")
//	EMBEDDING_BASE_URL  (required for azure)
//	EMBEDDING_API_VERSION (optional for azure)
//
// Run with: go test -run TestEmbedding -v -count=1 ./sdkx/embedding/

func newTestEmbedder(t *testing.T) embedding.Embedder {
	t.Helper()
	provider := os.Getenv("EMBEDDING_PROVIDER")
	apiKey := os.Getenv("EMBEDDING_API_KEY")
	model := os.Getenv("EMBEDDING_MODEL")
	if provider == "" || apiKey == "" {
		t.Skip("skipping embedding integration test: EMBEDDING_PROVIDER and EMBEDDING_API_KEY required")
	}

	config := map[string]any{
		"api_key": apiKey,
	}
	if v := os.Getenv("EMBEDDING_BASE_URL"); v != "" {
		config["base_url"] = v
	}
	if v := os.Getenv("EMBEDDING_API_VERSION"); v != "" {
		config["api_version"] = v
	}

	emb, err := embedding.NewFromConfig(provider, model, config)
	if err != nil {
		t.Fatalf("NewFromConfig(%q, %q): %v", provider, model, err)
	}
	return emb
}

func TestEmbedding_SingleText(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vec, err := emb.Embed(ctx, "你好，世界")
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(vec) == 0 {
		t.Fatal("Embed returned empty vector")
	}
	t.Logf("vector dimension: %d", len(vec))

	hasNonZero := false
	for _, v := range vec {
		if v != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Fatal("all vector components are zero")
	}
}

func TestEmbedding_VectorDimension(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v1, err := emb.Embed(ctx, "first text")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	v2, err := emb.Embed(ctx, "completely different second text with more words")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(v1) != len(v2) {
		t.Fatalf("dimension mismatch: %d vs %d", len(v1), len(v2))
	}
	t.Logf("consistent dimension: %d", len(v1))
}

func TestEmbedding_SimilarTexts_HighCosine(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v1, err := emb.Embed(ctx, "我喜欢吃苹果")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	v2, err := emb.Embed(ctx, "我爱吃水果")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	vFar, err := emb.Embed(ctx, "量子力学的基本原理是什么")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	simClose := cosine(v1, v2)
	simFar := cosine(v1, vFar)

	t.Logf("similar texts cosine: %.4f", simClose)
	t.Logf("distant texts cosine: %.4f", simFar)

	if simClose <= simFar {
		t.Fatalf("similar texts should have higher cosine (%.4f) than distant texts (%.4f)", simClose, simFar)
	}
	if simClose < 0.5 {
		t.Fatalf("similar texts cosine %.4f is unexpectedly low", simClose)
	}
}

func TestEmbedding_Batch(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	texts := []string{
		"The quick brown fox",
		"jumps over the lazy dog",
		"你好世界",
	}

	vecs, err := emb.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
	}

	dim := len(vecs[0])
	for i, v := range vecs {
		if len(v) == 0 {
			t.Fatalf("vector %d is empty", i)
		}
		if len(v) != dim {
			t.Fatalf("vector %d dimension %d != expected %d", i, len(v), dim)
		}
	}
	t.Logf("batch: %d vectors, dimension %d", len(vecs), dim)
}

func TestEmbedding_BatchEmpty(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	vecs, err := emb.EmbedBatch(ctx, nil)
	if err != nil {
		t.Fatalf("EmbedBatch(nil): %v", err)
	}
	if vecs != nil {
		t.Fatalf("expected nil for empty input, got %d vectors", len(vecs))
	}

	vecs2, err := emb.EmbedBatch(ctx, []string{})
	if err != nil {
		t.Fatalf("EmbedBatch([]): %v", err)
	}
	if vecs2 != nil {
		t.Fatalf("expected nil for empty slice, got %d vectors", len(vecs2))
	}
}

func TestEmbedding_BatchConsistency(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	text := "consistency check"

	single, err := emb.Embed(ctx, text)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	batch, err := emb.EmbedBatch(ctx, []string{text})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(batch))
	}

	sim := cosine(single, batch[0])
	t.Logf("single vs batch cosine: %.6f", sim)
	if sim < 0.999 {
		t.Fatalf("single and batch results should be nearly identical, cosine=%.6f", sim)
	}
}

func TestEmbedding_Concurrent(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 5
	texts := []string{
		"concurrent test one",
		"concurrent test two",
		"concurrent test three",
		"concurrent test four",
		"concurrent test five",
	}

	results := make([][]float32, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = emb.Embed(ctx, texts[idx])
		}(i)
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if len(results[i]) == 0 {
			t.Errorf("goroutine %d: empty vector", i)
		}
	}
}

func TestEmbedding_UnicodeAndEmoji(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	texts := []string{
		"日本語テスト",
		"한국어 테스트",
		"Ελληνικά δοκιμή",
	}

	for _, text := range texts {
		vec, err := emb.Embed(ctx, text)
		if err != nil {
			t.Errorf("Embed(%q): %v", text, err)
			continue
		}
		if len(vec) == 0 {
			t.Errorf("Embed(%q): empty vector", text)
		}
	}
}

func TestEmbedding_LongText(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	long := ""
	for range 200 {
		long += "这是一段用于测试长文本嵌入的句子。"
	}
	t.Logf("long text length: %d chars", len([]rune(long)))

	vec, err := emb.Embed(ctx, long)
	if err != nil {
		t.Fatalf("Embed long text: %v", err)
	}
	if len(vec) == 0 {
		t.Fatal("empty vector for long text")
	}
}

func TestEmbedding_VectorNormalized(t *testing.T) {
	emb := newTestEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vec, err := emb.Embed(ctx, "normalization check")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	norm := l2norm(vec)
	t.Logf("L2 norm: %.6f", norm)

	// OpenAI/Azure embedding models return L2-normalized vectors (norm ≈ 1.0)
	if math.Abs(float64(norm)-1.0) > 0.01 {
		t.Fatalf("expected L2 norm ≈ 1.0, got %.6f", norm)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func l2norm(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}
