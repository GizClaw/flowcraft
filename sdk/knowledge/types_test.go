package knowledge

import "testing"

func TestDefaultChunkConfig(t *testing.T) {
	cfg := DefaultChunkConfig()
	if cfg.ChunkSize != 512 {
		t.Fatalf("expected 512, got %d", cfg.ChunkSize)
	}
	if cfg.ChunkOverlap != 64 {
		t.Fatalf("expected 64, got %d", cfg.ChunkOverlap)
	}
}

func TestContextLayers(t *testing.T) {
	if LayerAbstract != "L0" {
		t.Fatalf("expected L0, got %s", LayerAbstract)
	}
	if LayerOverview != "L1" {
		t.Fatalf("expected L1, got %s", LayerOverview)
	}
	if LayerDetail != "L2" {
		t.Fatalf("expected L2, got %s", LayerDetail)
	}
}

func TestDerivedSigIsStaleComparesEmbedSigStrictly(t *testing.T) {
	got := DerivedSig{SourceVer: 1, ChunkerSig: "chunker:v1", EmbedSig: "embed:v1"}
	want := DerivedSig{SourceVer: 1, ChunkerSig: "chunker:v1"}
	if !got.IsStale(want) {
		t.Fatalf("expected vectors to be stale when desired EmbedSig is empty")
	}
	if !want.IsStale(got) {
		t.Fatalf("expected missing vectors to be stale when desired EmbedSig is set")
	}
}
