package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// TestChunkerRefusesTextAboveItsCeiling pins the derivation gate: a document
// whose text is above the ceiling is refused before the text is joined, and the
// same document one rune shorter derives. The refusal has to be a sentinel the
// worker can recognize -- the document is stored, so the pass that reads it
// again reaches the same answer and has to step over it rather than retry it.
func TestChunkerRefusesTextAboveItsCeiling(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 64, MaxDocumentRunes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chunker.Derive(context.Background(), documentArtifact(strings.Repeat("a", 101))); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("oversized document error = %v, want the too-large sentinel", err)
	}
	derived, err := chunker.Derive(context.Background(), documentArtifact(strings.Repeat("a", 100)))
	if err != nil {
		t.Fatalf("a document at the ceiling was refused: %v", err)
	}
	if len(artifactsOfKind(derived, KindChunk)) == 0 {
		t.Fatalf("a document at the ceiling derived no chunks: %#v", derived)
	}
}

// TestChunkerCountsTheTextItReadsAndNotThePartsItDoesNot pins what the ceiling
// measures: the text a deriver reads, summed over text parts, with media that
// rides along for storage left out. A document of images derives nothing, so it
// cannot be above a derivation ceiling.
func TestChunkerCountsTheTextItReadsAndNotThePartsItDoesNot(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 64, MaxDocumentRunes: 100})
	if err != nil {
		t.Fatal(err)
	}
	split := documentArtifact(strings.Repeat("a", 60))
	split.Content.Parts = append(split.Content.Parts, coremessage.TextPart{Text: strings.Repeat("b", 60)})
	if _, err := chunker.Derive(context.Background(), split); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("two 60-rune parts error = %v, want the too-large sentinel", err)
	}
	withMedia := documentArtifact(strings.Repeat("a", 100))
	withMedia.Content.Parts = append(withMedia.Content.Parts, coremessage.ImagePart{
		Source: imageOf(t, strings.Repeat("x", 4096)),
	})
	if _, err := chunker.Derive(context.Background(), withMedia); err != nil {
		t.Fatalf("media was counted against the text ceiling: %v", err)
	}
}

// TestChunkerRefusesABuildAboveItsArtifactCeiling pins the second bound: the
// text ceiling bounds the work only as long as a document turns into a sane
// number of items. A chunk size of one rune turns any text into one item per
// rune, which is a build nobody can serve -- the refusal arrives while the
// build is being appended, not after it is complete.
func TestChunkerRefusesABuildAboveItsArtifactCeiling(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("a", MaxDocumentArtifacts+1)
	if _, err := chunker.Derive(context.Background(), documentArtifact(text)); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("one-rune chunks error = %v, want the too-large sentinel", err)
	}
}

// TestChunkerRefusesASectionPerLine pins the same bound on structure: headings
// are what the build multiplies by before any chunking happens, so a document
// that is nothing but headings is refused in the split rather than after the
// sections have been materialized.
func TestChunkerRefusesASectionPerLine(t *testing.T) {
	if testing.Short() {
		t.Skip("builds one section per heading up to the ceiling")
	}
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 64})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for index := 0; index <= MaxDocumentArtifacts; index++ {
		text.WriteString("## heading\nbody\n")
	}
	if _, err := chunker.Derive(context.Background(), documentArtifact(text.String())); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("a section per line error = %v, want the too-large sentinel", err)
	}
	if _, err := parseSections("## one\nbody\n## two\nmore\n"); err != nil {
		t.Fatalf("a two-section document was refused: %v", err)
	}
}

// TestChunkerCeilingCannotExceedTheContract pins the one direction the config
// may not go: a deployment bounds documents inside the contract's ceiling, it
// does not raise it.
func TestChunkerCeilingCannotExceedTheContract(t *testing.T) {
	if _, err := NewChunker(ChunkerConfig{MaxRunes: 64, MaxDocumentRunes: corememory.MaxDocumentRunes + 1}); err == nil {
		t.Fatal("a chunker above the contract ceiling was accepted")
	}
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if chunker.maxDocumentRunes != corememory.MaxDocumentRunes {
		t.Fatalf("an unnamed ceiling resolved to %d, want the contract's %d",
			chunker.maxDocumentRunes, corememory.MaxDocumentRunes)
	}
}

func imageOf(t *testing.T, payload string) media.ImageSource {
	t.Helper()
	source, err := media.NewImageBytes([]byte(payload), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// BenchmarkDeriveDocumentAtCeiling is what makes the ceiling a reading rather
// than a number: one document of the contract's maximum text, derived with the
// default chunking.
func BenchmarkDeriveDocumentAtCeiling(b *testing.B) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 1600, OverlapRunes: 160})
	if err != nil {
		b.Fatal(err)
	}
	line := strings.Repeat("memory is derived from what was said, not from what was stored. ", 11)
	paragraph := line + "\n\n"
	text := strings.Repeat(paragraph, corememory.MaxDocumentRunes/len([]rune(paragraph)))
	input := documentArtifact(text)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := chunker.Derive(context.Background(), input); err != nil {
			b.Fatal(err)
		}
	}
}
