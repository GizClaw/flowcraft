package summary

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// extendFixture is one compactor over one summary store, with the store's
// substrate available so a test can say what an extension read and what it
// wrote.
type extendFixture struct {
	compactor *Compactor
	store     *summaryview.SummaryStore
	kv        *countingKV
	scope     corememory.Scope
}

type countingKV struct {
	storage.Store
	storage.PutIfAbsentStore

	gets int
	puts int
}

func (store *countingKV) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := store.Store.Get(ctx, key)
	if err == nil {
		store.gets++
	}
	return value, err
}

func (store *countingKV) Put(ctx context.Context, key string, data []byte) error {
	store.puts++
	return store.Store.Put(ctx, key, data)
}

func (store *countingKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	store.puts++
	return store.PutIfAbsentStore.PutIfAbsent(ctx, key, data)
}

func (store *countingKV) reset() { store.gets, store.puts = 0, 0 }

func newExtendFixture(t *testing.T, config Config, summarizer Summarizer) *extendFixture {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	rawKV, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	kvStore := &countingKV{Store: rawKV, PutIfAbsentStore: rawKV}
	store, err := summaryview.NewSummaryStore(logStore, kvStore, summaryview.WithClock(func() time.Time {
		return time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	}))
	if err != nil {
		t.Fatal(err)
	}
	compactor, err := New(config, store, summarizer)
	if err != nil {
		t.Fatal(err)
	}
	return &extendFixture{compactor: compactor, store: store, kv: kvStore, scope: testScope()}
}

func (fixture *extendFixture) compact(t *testing.T, inputs []Input) summaryview.Manifest {
	t.Helper()
	if _, err := fixture.compactor.Compact(context.Background(), CompactRequest{
		Scope: fixture.scope, ConversationID: "conversation", GenerationID: "generation",
		PolicySignature: "policy", Inputs: inputs,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, found, err := fixture.store.LoadActive(context.Background(), fixture.scope, "conversation")
	if err != nil || !found {
		t.Fatalf("active manifest = %#v, %v, %v", manifest, found, err)
	}
	return manifest
}

func (fixture *extendFixture) extend(t *testing.T, inputs []Input) (summaryview.Manifest, bool) {
	t.Helper()
	manifest, extended, err := fixture.compactor.Extend(context.Background(), ExtendRequest{
		Scope: fixture.scope, ConversationID: "conversation", GenerationID: "generation",
		PolicySignature: "policy", Inputs: inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, extended
}

// TestExtendMatchesACompleteCompaction is the equivalence the extension exists
// under: growing a published tree input by input names the same records, in the
// same order, as deriving the whole window at once -- including across the
// boundaries where a level appears, a group fills up, and a new group starts.
func TestExtendMatchesACompleteCompaction(t *testing.T) {
	waves := [][]Input{
		inputRange(0, 3),
		inputRange(3, 4),
		inputRange(7, 5),
		inputRange(12, 1),
		inputRange(13, 9),
		inputRange(22, 7),
	}
	configs := []Config{
		{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 1},
		{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 2},
		{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 3},
		{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 4},
		{ChunkSize: 3, CondenseThreshold: 5, GroupSize: 2, MaxDepth: 4},
	}
	for _, config := range configs {
		incremental := newExtendFixture(t, config, echoSummarizer{})
		manifest := incremental.compact(t, waves[0])
		for index, wave := range waves[1:] {
			extended, ok := incremental.extend(t, wave)
			if !ok {
				t.Fatalf("%+v: wave %d was not extendable", config, index+1)
			}
			manifest = extended
		}
		complete := newExtendFixture(t, config, echoSummarizer{})
		want := complete.compact(t, inputRange(0, 29))
		if !reflect.DeepEqual(manifest.RecordIDs, want.RecordIDs) {
			t.Fatalf("%+v: extended record ids = %#v, want %#v", config, manifest.RecordIDs, want.RecordIDs)
		}
		if !reflect.DeepEqual(manifest, want) {
			t.Fatalf("%+v: extended manifest = %#v, want %#v", config, manifest, want)
		}
		incrementalRecords := listActive(t, incremental, manifest)
		completeRecords := listActive(t, complete, want)
		if !reflect.DeepEqual(incrementalRecords, completeRecords) {
			t.Fatalf("%+v: extended records = %#v, want %#v", config, incrementalRecords, completeRecords)
		}
		if levels, ok := manifest.Tree(); !ok || len(levels) == 0 {
			t.Fatalf("%+v: extended tree = %#v, %v", config, levels, ok)
		}
	}
}

// TestExtendSplitsLikeACompleteCompaction pins the summarizer fallback: a group
// whose summary the model refuses is split, and an extension splits the same
// group the same way, because it derives the same children.
func TestExtendSplitsLikeACompleteCompaction(t *testing.T) {
	config := Config{ChunkSize: 4, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 4}
	incremental := newExtendFixture(t, config, &selectiveSummarizer{failAbove: 1})
	incremental.compact(t, inputRange(0, 3))
	extended, ok := incremental.extend(t, inputRange(3, 2))
	if !ok {
		t.Fatal("the wave was not extendable")
	}
	complete := newExtendFixture(t, config, &selectiveSummarizer{failAbove: 1})
	want := complete.compact(t, inputRange(0, 5))
	if !reflect.DeepEqual(extended, want) {
		t.Fatalf("extended manifest = %#v, want %#v", extended, want)
	}
	if !reflect.DeepEqual(listActive(t, incremental, extended), listActive(t, complete, want)) {
		t.Fatal("the split records differ from a complete compaction's")
	}
}

// TestExtendRefusesWhatItCannotExtend pins the answers that send a caller back
// to a complete compaction, and the one that means "nothing to do": a window
// the extension cannot place is refused, while inputs the manifest already
// covers leave the published manifest untouched.
func TestExtendRefusesWhatItCannotExtend(t *testing.T) {
	fixture := newExtendFixture(t, DefaultConfig(), echoSummarizer{})
	// No manifest at all: there is no window to extend.
	if _, ok := fixture.extend(t, inputRange(0, 2)); ok {
		t.Fatal("an extension without a published manifest was accepted")
	}
	manifest := fixture.compact(t, inputRange(0, 2))
	// The manifest already covers these inputs: the extension is a no-op, and a
	// no-op rewrites nothing.
	before := fixture.kv.puts
	again, ok := fixture.extend(t, inputRange(0, 2))
	if !ok {
		t.Fatal("an extension over covered inputs was refused")
	}
	if !reflect.DeepEqual(again, manifest) || fixture.kv.puts != before {
		t.Fatalf("covered inputs rewrote the manifest: %#v", again)
	}
	// An input that belongs below the window would move every group after it.
	below := inputRange(0, 4)
	below[3].CoverageRange = summaryview.CoverageRange{StartSeq: 1, EndSeq: 1}
	if _, ok := fixture.extend(t, below[3:]); ok {
		t.Fatal("an input below the window was accepted")
	}
	// The same input twice is not a window either.
	if _, ok := fixture.extend(t, []Input{
		inputRange(4, 1)[0], inputRange(4, 1)[0],
	}); ok {
		t.Fatal("a duplicated input was accepted")
	}
	// Another generation's manifest is not this generation's window.
	if _, extended, err := fixture.compactor.Extend(context.Background(), ExtendRequest{
		Scope: fixture.scope, ConversationID: "conversation", GenerationID: "other",
		PolicySignature: "policy", Inputs: inputRange(4, 1),
	}); err != nil || extended {
		t.Fatalf("another generation's window = %v, %v", extended, err)
	}
	// An extension that does not say which policy the tree was built with
	// cannot derive records the tree holds -- a record id covers the signature
	// -- so it is refused instead of publishing a tree of its own.
	if _, extended, err := fixture.compactor.Extend(context.Background(), ExtendRequest{
		Scope: fixture.scope, ConversationID: "conversation", GenerationID: "generation",
		Inputs: inputRange(4, 1),
	}); err == nil || extended {
		t.Fatalf("a signature-less extension = %v, %v", extended, err)
	}
	// A manifest published before the tree was recorded carries no levels to
	// place new inputs in.
	legacy := manifest.Clone()
	legacy.LevelCounts = nil
	legacy.FrontierDigest = "frontier"
	if err := fixture.store.PublishActive(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	if _, ok := fixture.extend(t, inputRange(4, 1)); ok {
		t.Fatal("a manifest without a tree was extended")
	}
}

// TestExtendReadsTheTailOnly is the reading that says why the extension exists:
// growing a published tree does not read the window below the change, so what
// one extension costs does not grow with the tree it extends.
func TestExtendReadsTheTailOnly(t *testing.T) {
	config := Config{ChunkSize: 4, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 4}
	var previous [2]int
	previousLeaves := 0
	for _, leaves := range []int{16, 64} {
		fixture := newExtendFixture(t, config, echoSummarizer{})
		fixture.compact(t, inputRange(0, leaves))
		fixture.kv.reset()
		manifest, ok := fixture.extend(t, inputRange(leaves, 3))
		if !ok {
			t.Fatalf("the extension into %d leaves was refused", leaves)
		}
		tree, _ := manifest.Tree()
		summaryRecords := 0
		for _, level := range tree {
			summaryRecords += len(level)
		}
		t.Logf("extension into %d leaves: %d record reads, %d writes for %d records",
			leaves, fixture.kv.gets, fixture.kv.puts, summaryRecords)
		if previous[0] > 0 {
			// The same extension into a four times larger tree: what an
			// extension reads and writes follows the change, not the tree. A
			// deeper tree costs one more group per level.
			if fixture.kv.gets > previous[0]+4 || fixture.kv.puts > previous[1]+4 {
				t.Fatalf("extending %d leaves cost %d reads and %d writes, %d leaves cost %d and %d",
					leaves, fixture.kv.gets, fixture.kv.puts, previousLeaves, previous[0], previous[1])
			}
		}
		if fixture.kv.gets > 24 {
			t.Fatalf("an extension into %d leaves read %d records", leaves, fixture.kv.gets)
		}
		previous, previousLeaves = [2]int{fixture.kv.gets, fixture.kv.puts}, leaves
	}
}

// listActive returns the active records in the manifest's order, as the text a
// test can compare across two stores.
func listActive(t *testing.T, fixture *extendFixture, manifest summaryview.Manifest) []string {
	t.Helper()
	records, err := fixture.store.ListActive(context.Background(), fixture.scope, "conversation",
		summaryview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]string, len(records))
	for _, record := range records {
		byID[record.ID] = fmt.Sprintf("%s|%s|%s", record.ID, record.Level, record.Text)
	}
	lines := make([]string, 0, len(manifest.RecordIDs))
	for _, id := range manifest.RecordIDs {
		line, ok := byID[id]
		if !ok {
			t.Fatalf("active record %q is missing", id)
		}
		lines = append(lines, line)
	}
	return lines
}

// inputRange builds count inputs whose message window starts at one more than
// start, so two ranges can be appended in order.
func inputRange(start, count int) []Input {
	result := make([]Input, 0, count)
	for index := start; index < start+count; index++ {
		result = append(result, Input{
			ID: fmt.Sprintf("fact-%02d", index), Text: fmt.Sprintf("fact %02d", index),
			Topics: []string{"topic"}, SourceRefs: []corememory.SourceRef{{
				Kind: corememory.SourceMessage, ID: fmt.Sprintf("conversation/message-%02d", index),
				Revision: fmt.Sprint(index + 1),
			}},
			CoverageRange: summaryview.CoverageRange{StartSeq: uint64(index + 1), EndSeq: uint64(index + 1)},
		})
	}
	return result
}
