package knowledge

import (
	"context"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

func TestChunkerParagraphUTF8OverlapAndStableID(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 8, OverlapRunes: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := documentArtifact("第一段\n\n第二段比较长\n\nthird")
	first, err := chunker.Derive(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	first = artifactsOfKind(first, KindDocumentChunk)
	if len(first) < 3 || first[0].Content.Text() != "第一段" {
		t.Fatalf("paragraph chunks = %#v", first)
	}
	for _, chunk := range first {
		text := chunk.Content.Text()
		if !utf8.ValidString(text) || len([]rune(text)) > 8 {
			t.Fatalf("invalid rune-safe chunk %q", text)
		}
		if chunk.Kind != KindDocumentChunk || !reflect.DeepEqual(chunk.Sources, input.Sources) {
			t.Fatalf("chunk contract = %#v", chunk)
		}
	}
	if got := []rune(first[1].Content.Text()); len(got) < 2 ||
		string(got[:2]) != tailRunes(first[0].Content.Text(), 2) {
		t.Fatalf("overlap missing: first=%q second=%q", first[0].Content.Text(), first[1].Content.Text())
	}
	reordered := input.Clone()
	reordered.Sources[0], reordered.Sources[1] = reordered.Sources[1], reordered.Sources[0]
	second, err := chunker.Derive(context.Background(), reordered)
	second = artifactsOfKind(second, KindDocumentChunk)
	if err != nil || len(second) != len(first) {
		t.Fatalf("reordered derive = %#v, %v", second, err)
	}
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("chunk %d ID changed after source reorder", index)
		}
	}
}

func artifactsOfKind(values []component.Artifact, kind component.ArtifactKind) []component.Artifact {
	result := make([]component.Artifact, 0, len(values))
	for _, value := range values {
		if value.Kind == kind {
			result = append(result, value)
		}
	}
	return result
}

func TestChunkerEmptyKindValidationAndClone(t *testing.T) {
	if _, err := NewChunker(ChunkerConfig{}); err == nil {
		t.Fatal("zero max accepted")
	}
	if _, err := NewChunker(ChunkerConfig{MaxRunes: 4, OverlapRunes: 4}); err == nil {
		t.Fatal("invalid overlap accepted")
	}
	chunker, _ := NewChunker(ChunkerConfig{MaxRunes: 4})
	empty := documentArtifact(" \n\t ")
	got, err := chunker.Derive(context.Background(), empty)
	if err != nil || !reflect.DeepEqual(got, []component.Artifact{}) {
		t.Fatalf("empty document = %#v, %v", got, err)
	}
	wrong := documentArtifact("text")
	wrong.Kind = KindRawForTest
	if _, err := chunker.Derive(context.Background(), wrong); err == nil {
		t.Fatal("wrong kind accepted")
	}
	noDocumentSource := documentArtifact("text")
	noDocumentSource.Sources = noDocumentSource.Sources[:1]
	if _, err := chunker.Derive(context.Background(), noDocumentSource); err == nil {
		t.Fatal("missing document source provenance accepted")
	}
	input := documentArtifact("abcdef")
	chunks, err := chunker.Derive(context.Background(), input)
	chunks = artifactsOfKind(chunks, KindDocumentChunk)
	if err != nil {
		t.Fatal(err)
	}
	input.Sources[0].ID = "mutated"
	input.Metadata["key"] = "mutated"
	chunks[0].Sources[0].ID = "output mutation"
	if chunks[1].Sources[0].ID != "external" || chunks[1].Metadata["key"] != "value" {
		t.Fatal("chunk outputs alias each other or input")
	}
}

const KindRawForTest component.ArtifactKind = "raw_message"

func documentArtifact(text string) component.Artifact {
	return component.Artifact{
		Kind: KindDocument, ID: "document",
		Content: sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}},
		Sources: []sdkmemory.SourceRef{
			{Kind: sdkmemory.SourceExternal, ID: "external", Revision: "7"},
			{Kind: sdkmemory.SourceDocument, ID: "document", Revision: "3"},
		},
		Metadata: sdkmemory.Metadata{"key": "value"},
	}
}

func tailRunes(value string, count int) string {
	runes := []rune(value)
	if count > len(runes) {
		count = len(runes)
	}
	return string(runes[len(runes)-count:])
}
