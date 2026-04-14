package knowledge

import (
	"strings"
	"testing"
)

func TestChunkDocument_Short(t *testing.T) {
	chunks := ChunkDocument("test.md", "short text", DefaultChunkConfig())
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Content != "short text" {
		t.Fatalf("content mismatch: %q", chunks[0].Content)
	}
	if chunks[0].DocName != "test.md" {
		t.Fatalf("doc name mismatch: %q", chunks[0].DocName)
	}
}

func TestChunkDocument_Empty(t *testing.T) {
	chunks := ChunkDocument("test.md", "", DefaultChunkConfig())
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks, got %d", len(chunks))
	}
}

func TestChunkDocument_MultipleChunks(t *testing.T) {
	content := strings.Repeat("word ", 200) // ~1000 chars
	chunks := ChunkDocument("test.md", content, ChunkConfig{ChunkSize: 200, ChunkOverlap: 50})
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d: expected index %d, got %d", i, i, c.Index)
		}
		if c.DocName != "test.md" {
			t.Fatalf("chunk %d: wrong docName", i)
		}
	}
}

func TestChunkDocument_OverlapTooLarge(t *testing.T) {
	content := strings.Repeat("a", 200)
	chunks := ChunkDocument("test.md", content, ChunkConfig{ChunkSize: 100, ChunkOverlap: 100})
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
}

func TestFindBreak(t *testing.T) {
	// The ". " must occur in the last quarter of the string
	text := "aaaa bbbb. cccc dddd eeee ffff. gggg"
	idx := findBreak(text, ". ")
	if idx < 0 {
		t.Fatal("expected to find break point")
	}
	// The second ". " at position ~30 out of ~36 should be in the last quarter
	if text[idx:idx+2] != ". " {
		t.Fatalf("break at wrong position: %d -> %q", idx, text[idx:])
	}
}
