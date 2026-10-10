package summary

import (
	"reflect"
	"testing"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestBuildManifestIsAFunctionOfTheRecords pins the property that lets a
// replaced generation be published again: whatever order the records come in,
// and whoever reads them, one generation's records describe one manifest.
func TestBuildManifestIsAFunctionOfTheRecords(t *testing.T) {
	records := []Record{
		summaryRecord("leaf-2", L0, 2, "generation-1"),
		summaryRecord("rollup", L1, 1, "generation-1"),
		summaryRecord("leaf-1", L0, 1, "generation-1"),
	}
	manifest, err := BuildManifest(summaryScope, "conversation", "generation-1", records)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"leaf-1", "leaf-2", "rollup"}; !reflect.DeepEqual(manifest.RecordIDs, want) {
		t.Fatalf("record ids = %#v, want %#v", manifest.RecordIDs, want)
	}
	if manifest.CoverageRange != (CoverageRange{StartSeq: 1, EndSeq: 2}) {
		t.Fatalf("coverage = %#v, want the leaves' window", manifest.CoverageRange)
	}
	if manifest.GenerationID != "generation-1" || manifest.FrontierDigest == "" {
		t.Fatalf("manifest = %#v", manifest)
	}

	shuffled := []Record{records[1], records[0], records[2]}
	again, err := BuildManifest(summaryScope, "conversation", "generation-1", shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest, again) {
		t.Fatalf("shuffled input built %#v, want %#v", again, manifest)
	}
}

func TestBuildManifestRejectsIncompleteAndForeignInputs(t *testing.T) {
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1", nil); err == nil {
		t.Fatal("a manifest without records was built")
	}
	foreign := summaryRecord("leaf", L0, 1, "generation-1")
	foreign.ConversationID = "other"
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1", []Record{foreign}); err == nil {
		t.Fatal("a manifest over another conversation's record was built")
	}
	if _, err := BuildManifest(summaryScope, "conversation", "", []Record{foreign}); err == nil {
		t.Fatal("a manifest without a generation was built")
	}
}

func summaryRecord(id string, level Level, seq uint64, generation string) Record {
	return Record{
		ID: id, Scope: summaryScope, ConversationID: "conversation", Level: level,
		Text: id, Content: textContent(id), InputIDs: []string{"input-" + id},
		SourceRefs:    []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message"}},
		CoverageRange: CoverageRange{StartSeq: seq, EndSeq: seq},
		SourceDigest:  "source-" + id, TransformSignature: "transform-v1",
		GenerationID: generation, CreatedAt: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
	}
}
