package knowledge

import (
	"context"
	"testing"
	"unicode/utf8"
)

func TestHierarchyMarkdownSectionsAndStableIDs(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 8, OverlapRunes: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := documentArtifact("# Alpha\n第一段很长文字\n## Beta\n第二段abcdef\n# Gamma\n末尾")
	first, err := chunker.Derive(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 7 {
		t.Fatalf("records = %#v", first)
	}
	if first[0].Kind != KindResource || first[0].Metadata["level"] != "0" {
		t.Fatalf("resource = %#v", first[0])
	}
	var alphaID, betaID string
	for _, record := range first {
		switch record.Metadata["title"] {
		case "Alpha":
			alphaID = record.ID
		case "Beta":
			betaID = record.ID
			if record.Metadata["parent_id"] != alphaID || record.Metadata["level"] != "2" {
				t.Fatalf("nested section = %#v", record)
			}
		}
		if record.Kind == KindChunk {
			if !utf8.ValidString(record.Content.Text()) || len([]rune(record.Content.Text())) > 8 {
				t.Fatalf("unsafe chunk %q", record.Content.Text())
			}
			if record.Metadata["parent_id"] == "" {
				t.Fatalf("chunk without section parent: %#v", record)
			}
		}
	}
	if alphaID == "" || betaID == "" {
		t.Fatalf("section IDs missing: %#v", first)
	}
	second, err := chunker.Derive(context.Background(), input)
	if err != nil || len(second) != len(first) {
		t.Fatalf("second derive = %#v, %v", second, err)
	}
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("record %d ID changed", index)
		}
	}
}

func TestHierarchyCreatesRootSectionAndChunksNeverCrossSections(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{MaxRunes: 64, OverlapRunes: 4})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := chunker.Derive(context.Background(), documentArtifact("没有标题的内容"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 3 || plain[1].Kind != KindSection || plain[1].Metadata["title"] != "" ||
		plain[2].Metadata["parent_id"] != plain[1].ID {
		t.Fatalf("root hierarchy = %#v", plain)
	}

	records, err := chunker.Derive(context.Background(), documentArtifact("# One\none body\n# Two\ntwo body"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Kind == KindChunk && record.Content.Text() != "one body" && record.Content.Text() != "two body" {
			t.Fatalf("chunk crossed section: %#v", record)
		}
	}
}

func TestHierarchyRevisionIsolationAndTypedSummaries(t *testing.T) {
	chunker, err := NewChunker(ChunkerConfig{
		MaxRunes: 16, OverlapRunes: 2,
		Summary: SummaryConfig{Document: true, Sections: true, MaxRunes: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := documentArtifact("# Heading\nabcdefgh")
	first, err := chunker.Derive(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	summaries := artifactsOfKind(first, KindSummary)
	if len(summaries) != 2 || len([]rune(summaries[0].Content.Text())) > 4 ||
		summaries[0].Metadata["parent_id"] == "" || summaries[0].Metadata["transform_signature"] == "" {
		t.Fatalf("summaries = %#v", summaries)
	}
	revised := input.Clone()
	for index := range revised.Sources {
		if revised.Sources[index].Kind == "document" {
			revised.Sources[index].Revision = "4"
		}
	}
	second, err := chunker.Derive(context.Background(), revised)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ID == second[0].ID || first[0].Metadata["source_digest"] == second[0].Metadata["source_digest"] {
		t.Fatal("source revision reused hierarchy identity")
	}
}

func TestHierarchyIgnoresHeadingsInsideMarkdownFences(t *testing.T) {
	chunker, _ := NewChunker(ChunkerConfig{MaxRunes: 64})
	records, err := chunker.Derive(context.Background(), documentArtifact("Real\n====\n```\n# Not a heading\n```"))
	if err != nil {
		t.Fatal(err)
	}
	sections := artifactsOfKind(records, KindSection)
	if len(sections) != 1 || sections[0].Metadata["title"] != "Real" {
		t.Fatalf("fenced headings parsed as sections: %#v", sections)
	}
}
