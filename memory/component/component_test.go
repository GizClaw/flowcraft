package component

import (
	"reflect"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestArtifactValidateAndCloneOwnership(t *testing.T) {
	artifact := Artifact{
		Kind:    "chunk",
		ID:      "stable-id",
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "original"}}},
		Sources: []corememory.SourceRef{{Kind: corememory.SourceDocument, ID: "doc"}},
		Metadata: corememory.Metadata{
			"key": "original",
		},
	}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	cloned := artifact.Clone()
	cloned.Content.Parts[0] = coremessage.TextPart{Text: "changed"}
	cloned.Sources[0].ID = "changed"
	cloned.Metadata["key"] = "changed"
	if artifact.Content.Text() != "original" || artifact.Sources[0].ID != "doc" || artifact.Metadata["key"] != "original" {
		t.Fatalf("clone aliases artifact: %#v", artifact)
	}

	bad := artifact
	bad.Sources = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("artifact without provenance was accepted")
	}
}

func TestCandidateRetainsLaneNativeScore(t *testing.T) {
	candidate := Candidate{
		ID:     "candidate",
		Lane:   "bm25",
		Name:   "messages",
		Score:  -17.5,
		Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"},
	}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidate.Score, -17.5) {
		t.Fatalf("score changed to %v", candidate.Score)
	}
}
