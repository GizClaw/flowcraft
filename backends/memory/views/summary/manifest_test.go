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
		summaryRollup("rollup", L1, 1, []string{"leaf-1", "leaf-2"}, "generation-1"),
		summaryRecord("leaf-1", L0, 1, "generation-1"),
	}
	manifest, err := BuildManifest(summaryScope, "conversation", "generation-1", records)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"leaf-1", "leaf-2", "rollup"}; !reflect.DeepEqual(manifest.RecordIDs, want) {
		t.Fatalf("record ids = %#v, want %#v", manifest.RecordIDs, want)
	}
	if want := []int{2, 1, 0, 0}; !reflect.DeepEqual(manifest.LevelCounts, want) {
		t.Fatalf("level counts = %#v, want %#v", manifest.LevelCounts, want)
	}
	tree, ok := manifest.Tree()
	if !ok || !reflect.DeepEqual(tree, [][]string{{"leaf-1", "leaf-2"}, {"rollup"}}) {
		t.Fatalf("tree = %#v, %v", tree, ok)
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

// TestBuildManifestPlacesGroupsByTheirFirstChild pins the placement rule a
// writer that extends a published tree relies on: a group sits where its first
// child sits, and a level that does not cover a consecutive run of the level
// below it is not a tree.
func TestBuildManifestPlacesGroupsByTheirFirstChild(t *testing.T) {
	leaves := []Record{
		summaryRecord("leaf-1", L0, 1, "generation-1"),
		summaryRecord("leaf-2", L0, 2, "generation-1"),
		summaryRecord("leaf-3", L0, 3, "generation-1"),
		summaryRecord("leaf-4", L0, 4, "generation-1"),
	}
	// Two groups whose coverage starts at the same message sequence: only the
	// children say which one comes first.
	second := summaryRollup("group-b", L1, 1, []string{"leaf-3", "leaf-4"}, "generation-1")
	first := summaryRollup("group-a", L1, 1, []string{"leaf-1", "leaf-2"}, "generation-1")
	manifest, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{leaves[2], leaves[3], second, leaves[0], leaves[1], first})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"leaf-1", "leaf-2", "leaf-3", "leaf-4", "group-a", "group-b"}; !reflect.DeepEqual(manifest.RecordIDs, want) {
		t.Fatalf("record ids = %#v, want %#v", manifest.RecordIDs, want)
	}

	broken := summaryRollup("group-c", L1, 1, []string{"leaf-2", "leaf-4"}, "generation-1")
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{leaves[0], leaves[1], leaves[2], leaves[3], broken}); err == nil {
		t.Fatal("a group over a broken run was placed")
	}
	unknown := summaryRollup("group-d", L1, 1, []string{"leaf-1", "leaf-9"}, "generation-1")
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{leaves[0], leaves[1], unknown}); err == nil {
		t.Fatal("a group over an unknown child was placed")
	}
	orphan := summaryRecord("rollup", L2, 1, "generation-1")
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{leaves[0], leaves[1], orphan}); err == nil {
		t.Fatal("a level without the level below it was placed")
	}
}

// TestManifestValidateChecksTheTree pins what a manifest with a tree has to
// say: every level accounted for, nothing below an empty level, and a leaf for
// every record list.
func TestManifestValidateChecksTheTree(t *testing.T) {
	manifest := Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
		RecordIDs: []string{"leaf", "rollup"}, LevelCounts: []int{1, 1, 0, 0},
		CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 1}, FrontierDigest: "frontier",
		PublishedAt: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid manifest = %v", err)
	}
	short := manifest
	short.LevelCounts = []int{1, 1}
	if err := short.Validate(); err == nil {
		t.Fatal("a manifest naming two of four levels was accepted")
	}
	shortfall := manifest
	shortfall.LevelCounts = []int{1, 1, 0, 1}
	if err := shortfall.Validate(); err == nil {
		t.Fatal("a manifest with records below an empty level was accepted")
	}
	miscount := manifest
	miscount.LevelCounts = []int{1, 0, 0, 0}
	if err := miscount.Validate(); err == nil {
		t.Fatal("a manifest counting fewer records than it stores was accepted")
	}
	if _, ok := miscount.Tree(); ok {
		t.Fatal("a manifest whose counts do not add up returned a tree")
	}
	// A manifest published before the tree was recorded is still valid: its
	// records are the active ones, and it simply reports no tree.
	tree := manifest
	tree.LevelCounts = nil
	if err := tree.Validate(); err != nil {
		t.Fatalf("a manifest without counts = %v", err)
	}
	if _, ok := tree.Tree(); ok {
		t.Fatal("a manifest without counts returned a tree")
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
	// Two records cannot be the same record, and a level cannot be named twice
	// over different children.
	duplicate := summaryRecord("leaf", L0, 1, "generation-1")
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{duplicate, duplicate}); err == nil {
		t.Fatal("a tree naming one record twice was built")
	}
	twice := summaryRollup("group", L1, 1, []string{"leaf"}, "generation-1")
	second := summaryRollup("group-2", L1, 1, []string{"leaf"}, "generation-1")
	if _, err := BuildManifest(summaryScope, "conversation", "generation-1",
		[]Record{summaryRecord("leaf", L0, 1, "generation-1"), twice, second}); err == nil {
		t.Fatal("two groups claiming one child were placed")
	}
}

func summaryRecord(id string, level Level, seq uint64, generation string) Record {
	record := summaryRollup(id, level, seq, []string{"input-" + id}, generation)
	return record
}

func summaryRollup(id string, level Level, seq uint64, children []string, generation string) Record {
	return Record{
		ID: id, Scope: summaryScope, ConversationID: "conversation", Level: level,
		Text: id, Content: textContent(id), InputIDs: children,
		SourceRefs:    []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message"}},
		CoverageRange: CoverageRange{StartSeq: seq, EndSeq: seq},
		SourceDigest:  "source-" + id, TransformSignature: "transform-v1",
		GenerationID: generation, CreatedAt: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
	}
}
