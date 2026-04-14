package knowledge

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func BenchmarkSearchDataset_10(b *testing.B) {
	benchmarkSearch(b, 10)
}

func BenchmarkSearchDataset_100(b *testing.B) {
	benchmarkSearch(b, 100)
}

func BenchmarkSearchDataset_1000(b *testing.B) {
	benchmarkSearch(b, 1000)
}

func benchmarkSearch(b *testing.B, numDocs int) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	for i := 0; i < numDocs; i++ {
		content := fmt.Sprintf("Document %d contains information about topic %d with various keywords and technical details about programming systems", i, i%50)
		_ = store.AddDocument(ctx, "bench", fmt.Sprintf("doc%d.md", i), content)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.Search(ctx, "bench", "programming systems", SearchOptions{TopK: 5})
	}
}

func BenchmarkBM25Score(b *testing.B) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("Go is a statically typed compiled programming language"))
	cs.AddDocument(tok.Tokenize("Python is a high-level general-purpose programming language"))

	chunk := &Chunk{Content: "Go programming language features and concurrency model"}
	keywords := ExtractKeywords("Go programming concurrency", tok)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ScoreChunk(chunk, keywords, cs, tok)
	}
}

func BenchmarkTokenize_Simple(b *testing.B) {
	tok := &SimpleTokenizer{}
	text := "Go is a statically typed compiled programming language designed at Google for system programming"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tok.Tokenize(text)
	}
}

func BenchmarkTokenize_CJK(b *testing.B) {
	tok := &CJKTokenizer{}
	text := "Go语言是一种静态类型的编译型编程语言用于系统编程"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tok.Tokenize(text)
	}
}
