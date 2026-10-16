package worker

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// summaryPassFixture is one derivation pipeline over a real workspace: the real
// message store, the real fact and summary views and the real compactor, so a
// reading covers what a deployment pays.
type summaryPassFixture struct {
	messages  *msgsource.MessageStore
	facts     *factview.FactStore
	summaries *summaryview.SummaryStore
	compactor *summaryderive.Compactor
	processor *Processor
	kv        *countingStore
	log       *countingLog
	scope     corememory.Scope
}

// newSummaryPassFixture builds the pipeline. withCompactor=false builds the same
// pipeline with the summary branch switched off, which is how a reading
// attributes a pass's work to it.
func newSummaryPassFixture(t *testing.T, withCompactor bool, deriver component.Deriver) *summaryPassFixture {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	rawLog, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	rawKV, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore := &countingLog{Log: rawLog, CommitLog: rawLog}
	kvStore := &countingStore{Store: rawKV, CASStore: rawKV, PutIfAbsentStore: rawKV}
	clock := func() time.Time { return testLeaseNow() }
	messages, err := msgsource.NewMessageStore(logStore, msgsource.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kvStore, factview.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := summaryview.NewSummaryStore(logStore, kvStore, summaryview.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	var compactor *summaryderive.Compactor
	if withCompactor {
		compactor, err = summaryderive.New(summaryderive.DefaultConfig(), summaries,
			summaryderive.ExtractiveSummarizer{})
		if err != nil {
			t.Fatal(err)
		}
	}
	processor, err := NewProcessor(Config{
		Messages: messages, Facts: facts, Deriver: deriver, Compactor: compactor,
		Checkpoints: newMemoryCheckpoints(), Projection: "test", PolicyDigest: "digest-summary",
		Indexers: []ProjectionIndexer{{Name: "test", Indexer: &countingIndexer{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &summaryPassFixture{
		messages: messages, facts: facts, summaries: summaries, compactor: compactor,
		processor: processor, kv: kvStore, log: logStore,
		scope: corememory.Scope{RuntimeID: "memories", UserID: "u1"},
	}
}

// turn appends one turn and derives it, which is what a write path plus a poll
// does.
func (fixture *summaryPassFixture) turn(t *testing.T, index int, text string) {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.messages.Commit(ctx, msgsource.AppendRequest{
		Scope: fixture.scope, ConversationID: "conv-a", IdempotencyKey: fmt.Sprintf("turn-%d", index),
		Messages: []coremessage.Message{
			coremessage.NewTextMessage(coremessage.RoleUser, text),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.processor.ProcessConversation(ctx, fixture.scope, "conv-a"); err != nil {
		t.Fatal(err)
	}
}

func (fixture *summaryPassFixture) manifest(t *testing.T) summaryview.Manifest {
	t.Helper()
	manifest, found, err := fixture.summaries.LoadActive(context.Background(), fixture.scope, "conv-a")
	if err != nil || !found {
		t.Fatalf("active manifest = %#v, found=%v, err=%v", manifest, found, err)
	}
	return manifest
}

// compactWholeWindow compacts the conversation's complete fact window in one
// pass, which is the manifest every extension has to agree with.
func (fixture *summaryPassFixture) compactWholeWindow(t *testing.T) summaryview.Manifest {
	t.Helper()
	ctx := context.Background()
	values, err := fixture.facts.List(ctx, fixture.scope, "conv-a", factview.ListOptions{
		Generation: fixture.processor.PolicyDigest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]summaryderive.Input, 0, len(values))
	for _, value := range values {
		inputs = append(inputs, summaryderive.InputFromFact(value))
	}
	if _, err := fixture.compactor.Compact(ctx, summaryderive.CompactRequest{
		Scope: fixture.scope, ConversationID: "conv-a",
		GenerationID: fixture.processor.PolicyDigest(), PolicySignature: fixture.processor.PolicyDigest(),
		Inputs: inputs,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture.manifest(t)
}

// sameManifestExceptPublication compares what two manifests say about the tree,
// ignoring when each of them was published.
func sameManifestExceptPublication(t *testing.T, left, right summaryview.Manifest) {
	t.Helper()
	left.PublishedAt, right.PublishedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("manifests differ:\n got levels %v coverage %+v frontier %s\nwant levels %v coverage %+v frontier %s\nrecords: %s",
			left.LevelCounts, left.CoverageRange, left.FrontierDigest,
			right.LevelCounts, right.CoverageRange, right.FrontierDigest,
			strings.Join(firstDifference(left.RecordIDs, right.RecordIDs), "\n"))
	}
}

// firstDifference reports the records two manifests disagree about, as a short
// window around the first one.
func firstDifference(got, want []string) []string {
	if len(got) != len(want) {
		return []string{fmt.Sprintf("record counts %d and %d", len(got), len(want))}
	}
	for index := range got {
		if got[index] != want[index] {
			from := max(0, index-1)
			to := min(len(got), index+2)
			return []string{
				fmt.Sprintf("at %d of %d, got  %v", index, len(got), got[from:to]),
				fmt.Sprintf("at %d of %d, want %v", index, len(got), want[from:to]),
			}
		}
	}
	return []string{"none"}
}

// mutDeriver derives through hashDeriver, except for commits whose text says
// "silent": those derive no fact at all.
type muteDeriver struct{}

func (muteDeriver) Derive(ctx context.Context, source component.Artifact) ([]component.Artifact, error) {
	if strings.Contains(source.Content.Text(), "silent") {
		return nil, nil
	}
	return hashDeriver{}.Derive(ctx, source)
}

// TestSummaryPassExtendsThePublishedTree is the equivalence the pass relies on:
// deriving a conversation turn by turn publishes the manifest a complete
// compaction of the same window publishes, and the counters say which path each
// pass took. The run crosses the group size twice, so the tree grows levels
// while it is extended.
func TestSummaryPassExtendsThePublishedTree(t *testing.T) {
	fixture := newSummaryPassFixture(t, true, hashDeriver{})
	const turns = 70
	for index := 0; index < turns; index++ {
		fixture.turn(t, index, fmt.Sprintf("turn %d about a topic", index))
	}
	// The first pass of a conversation has no published tree to extend, so it
	// compacts the window; every pass after it appends to the tree.
	stats := fixture.processor.Stats()
	if stats.SummaryRebuilds != 1 || stats.SummaryExtensions != turns-1 {
		t.Fatalf("turn-by-turn passes = %#v", stats)
	}
	extended := fixture.manifest(t)
	sameManifestExceptPublication(t, extended, fixture.compactWholeWindow(t))
	levels, ok := extended.Tree()
	if !ok {
		t.Fatalf("published manifest names no tree: %#v", extended)
	}
	if len(levels) < 3 || len(levels[0]) != turns {
		t.Fatalf("tree = %#v", levels)
	}
}

// TestSummaryPassRebuildsWhenACommitChangesACoveredFact pins the fallback: a
// commit whose fact the fact view merges into a fact the tree already
// summarised makes the pass compact the window afresh, and the manifest that
// leaves behind is still the one a complete compaction publishes.
func TestSummaryPassRebuildsWhenACommitChangesACoveredFact(t *testing.T) {
	fixture := newSummaryPassFixture(t, true, muteDeriver{})
	fixture.turn(t, 0, "we talked about beverages")
	fixture.turn(t, 1, "another turn entirely")
	if stats := fixture.processor.Stats(); stats.SummaryRebuilds != 1 || stats.SummaryExtensions != 1 {
		t.Fatalf("passes before the merge = %#v", stats)
	}
	// The same content again: the fact view merges this turn's provenance into
	// the fact the tree already summarised, so that leaf -- and every group
	// above it -- no longer summarises the fact it names.
	fixture.turn(t, 2, "we talked about beverages")
	if stats := fixture.processor.Stats(); stats.SummaryRebuilds != 2 || stats.SummaryExtensions != 1 {
		t.Fatalf("a merged fact did not send the pass back to the window: %#v", stats)
	}
	sameManifestExceptPublication(t, fixture.manifest(t), fixture.compactWholeWindow(t))
	// A commit that derives no fact changes no summary at all.
	fixture.turn(t, 3, "a silent turn")
	if stats := fixture.processor.Stats(); stats.SummaryRebuilds != 2 || stats.SummaryExtensions != 1 {
		t.Fatalf("a commit without facts moved the summary branch: %#v", stats)
	}
}

// TestSummaryPassIsIdempotentOnARederivation pins the replay contract: a pass
// over commits whose facts the published tree already covers extends nothing and
// rewrites nothing, which is exactly what derive: fresh does to every
// conversation of a workspace.
func TestSummaryPassIsIdempotentOnARederivation(t *testing.T) {
	fixture := newSummaryPassFixture(t, true, hashDeriver{})
	const turns = 8
	for index := 0; index < turns; index++ {
		fixture.turn(t, index, fmt.Sprintf("turn %d about a topic", index))
	}
	published := fixture.manifest(t)
	// The same conversation again with the watermarks gone: every commit is
	// derived a second time, and every fact it derives is one the tree names.
	fixture.processor.checkpoints = newMemoryCheckpoints()
	if _, err := fixture.processor.ProcessConversation(context.Background(), fixture.scope, "conv-a"); err != nil {
		t.Fatal(err)
	}
	if stats := fixture.processor.Stats(); stats.SummaryRebuilds != 1 || stats.SummaryExtensions != 2*turns-1 {
		t.Fatalf("re-derivation passes = %#v", stats)
	}
	// A pass that derives nothing new rebuilds nothing: the manifest is the one
	// it found, publication time included.
	if again := fixture.manifest(t); !reflect.DeepEqual(again, published) {
		t.Fatalf("re-derivation published %#v\nwant %#v", again, published)
	}
	sameManifestExceptPublication(t, fixture.manifest(t), fixture.compactWholeWindow(t))
}

// TestSummaryPassCostFollowsTheCommit is the reading O7b-2 exists for: what one
// commit costs the summary branch follows the commit and the tail of the tree,
// not the conversation. The numbers are the difference the compactor makes to a
// pass -- the branch's share of the work -- which used to be a list of every
// fact in the conversation and a read of each of them.
func TestSummaryPassCostFollowsTheCommit(t *testing.T) {
	branch := make(map[int][2]int, 2)
	for _, turns := range []int{100, 400} {
		withBranch := newSummaryPassFixture(t, true, hashDeriver{})
		withoutBranch := newSummaryPassFixture(t, false, hashDeriver{})
		for index := 0; index < turns; index++ {
			withBranch.turn(t, index, fmt.Sprintf("turn %d", index))
			withoutBranch.turn(t, index, fmt.Sprintf("turn %d", index))
		}
		withBranch.kv.reset()
		withBranch.log.reset()
		withoutBranch.kv.reset()
		start := time.Now()
		withBranch.turn(t, turns, fmt.Sprintf("turn %d", turns))
		elapsed := time.Since(start)
		withoutBranch.turn(t, turns, fmt.Sprintf("turn %d", turns))
		t.Logf("pass at %d turns with the summary branch: %v; kv %d reads / %d B, %d list scans / %d entries / %d B, %d writes / %d B; log %d reads / %d records",
			turns, elapsed, withBranch.kv.gets, withBranch.kv.readBytes,
			withBranch.kv.lists, withBranch.kv.listEntries, withBranch.kv.listBytes,
			withBranch.kv.puts, withBranch.kv.writeBytes, withBranch.log.reads, withBranch.log.events)
		t.Logf("pass at %d turns without the summary branch: kv %d reads / %d B, %d list scans / %d entries / %d B",
			turns, withoutBranch.kv.gets, withoutBranch.kv.readBytes,
			withoutBranch.kv.lists, withoutBranch.kv.listEntries, withoutBranch.kv.listBytes)
		read := withBranch.kv.readBytes + withBranch.kv.listBytes -
			withoutBranch.kv.readBytes - withoutBranch.kv.listBytes
		scans := withBranch.kv.lists - withoutBranch.kv.lists
		branch[turns] = [2]int{read, scans}
		if stats := withBranch.processor.Stats(); stats.SummaryExtensions != int64(turns) {
			t.Fatalf("the marginal pass at %d turns did not extend the tree: %#v", turns, stats)
		}
		t.Logf("the summary branch's share at %d turns: %d B read, %d more list scans than the pass without it",
			turns, read, scans)
	}
	// The branch reads the tail of the tree it extends, never the window: a
	// conversation four times as long costs it no more than the manifest it has
	// to name again.
	if branch[400][0] > branch[100][0]+128<<10 {
		t.Fatalf("the summary branch read %d B at 400 turns and %d B at 100",
			branch[400][0], branch[100][0])
	}
}
