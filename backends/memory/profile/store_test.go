package profile

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// fact is one stored fact of the test conversation, as the pass would have read
// it: one entity, dated an hour before the test clock.
func fact(id, text string) factview.Fact {
	return testFact("conv-1", id, text, []string{"melanie"}, testNow.Add(-time.Hour))
}

// testKV returns an isolated KV substrate for one test.
func testKV(t *testing.T) storage.Store {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

func newTestStore(t *testing.T, kv storage.Store, config Config) *Store {
	t.Helper()
	store, err := NewStore(kv, WithClock(func() time.Time { return testNow }), WithConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// listOnlyKV hides the write extensions: the shape a substrate without them
// has, which a store that needs them has to refuse.
type listOnlyKV struct{ storage.Store }

// keyLessKV hides the optional key-only scan while keeping the write
// extensions, so the listing fallback runs on a store that could otherwise
// answer without values.
type keyLessKV struct {
	storage.Store
	storage.CASStore
	storage.PutIfAbsentStore
}

// TestBlockServesBothHalvesWithTheirProvenance pins what one request reads: the
// host's lines first, then the pass's, each naming where it came from.
func TestBlockServesBothHalvesWithTheirProvenance(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{Distill: true})
	scope := testScope()

	if _, err := store.SaveExplicit(ctx, scope, "conv-1", []string{"  Alice is a  counselor  "}); err != nil {
		t.Fatal(err)
	}
	entries := Distill([]factview.Fact{fact("fact-1", "Melanie loves pottery.")}, nil, store.Config())
	if len(entries) != 1 {
		t.Fatalf("Distill = %+v", entries)
	}
	if _, err := store.SaveDistilled(ctx, scope, "conv-1", entries, testNow); err != nil {
		t.Fatal(err)
	}

	block, found, err := store.Block(ctx, scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Block reported no block after two writes")
	}
	want := "- Alice is a counselor\n- Melanie loves pottery."
	if block.Text != want {
		t.Fatalf("block text = %q, want %q", block.Text, want)
	}
	if block.Entries != 2 || block.Truncated {
		t.Fatalf("block = %+v, want two entries and no truncation", block)
	}
	if len(block.Sources) != 2 {
		t.Fatalf("block sources = %+v, want one per entry", block.Sources)
	}
	for index, source := range block.Sources {
		if source.Kind != corememory.SourceExternal || !strings.HasPrefix(source.ID, "profile/conv-1/profile-entry-") {
			t.Errorf("source %d = %+v, want an external reference to the entry", index, source)
		}
	}
	if block.Sources[1].Locator != "facts:fact-1" {
		t.Errorf("distilled source locator = %q, want the fact behind the line", block.Sources[1].Locator)
	}
	if block.Sources[0].Locator != "" {
		t.Errorf("explicit source locator = %q, want none", block.Sources[0].Locator)
	}

	// A conversation without a block reports none rather than an empty one.
	if _, found, err := store.Block(ctx, scope, "conv-2"); err != nil || found {
		t.Fatalf("Block(conv-2) = found=%t err=%v, want no block", found, err)
	}
}

// TestBlockServingDropsTheLinesTheReadPathHides pins the block's half of a
// hide: the block is a reading of facts, so a read path that hides a fact must
// not serve the line a pass drew from it -- and the host's own lines stay,
// because a hide covers the corpus the views derived from, not what a writer
// put in the block by hand.
func TestBlockServingDropsTheLinesTheReadPathHides(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, testKV(t), Config{Distill: true})
	scope := testScope()
	if _, err := store.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor."}); err != nil {
		t.Fatal(err)
	}
	entries := Distill([]factview.Fact{
		fact("fact-1", "Melanie loves pottery."),
		fact("fact-2", "Melanie plays the cello."),
	}, nil, store.Config())
	if len(entries) != 2 {
		t.Fatalf("Distill = %+v", entries)
	}
	if _, err := store.SaveDistilled(ctx, scope, "conv-1", entries, testNow); err != nil {
		t.Fatal(err)
	}

	forgotten := map[string]struct{}{"fact-1": {}}
	asked := make([]string, 0, 2)
	block, hidden, err := store.BlockServing(ctx, scope, "conv-1", func(factID string) (bool, error) {
		asked = append(asked, factID)
		_, forget := forgotten[factID]
		return forget, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 1 || block.Entries != 2 {
		t.Fatalf("block = %+v hidden = %d", block, hidden)
	}
	want := "- Alice is a counselor.\n- Melanie plays the cello."
	if block.Text != want {
		t.Fatalf("block text = %q, want %q", block.Text, want)
	}
	if len(block.Sources) != 2 || block.Sources[1].Locator != "facts:fact-2" {
		t.Fatalf("sources = %+v", block.Sources)
	}
	// The filter is asked about the corpus, never about the host's line.
	if len(asked) != 2 || asked[0] != "fact-1" || asked[1] != "fact-2" {
		t.Fatalf("filter asked about %#v", asked)
	}

	// Hiding everything the block was drawn from leaves the host's half, and
	// a conversation with nothing left reports no block at all.
	everything, hidden, err := store.BlockServing(ctx, scope, "conv-1",
		func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 2 || everything.Text != "- Alice is a counselor." {
		t.Fatalf("block = %+v hidden = %d", everything, hidden)
	}
	empty := newTestStore(t, testKV(t), Config{Distill: true})
	if _, err := empty.SaveDistilled(ctx, scope, "conv-9", entries, testNow); err != nil {
		t.Fatal(err)
	}
	nothing, hidden, err := empty.BlockServing(ctx, scope, "conv-9",
		func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 2 || nothing.Entries != 0 || nothing.Text != "" {
		t.Fatalf("block = %+v hidden = %d, want no servable block", nothing, hidden)
	}

	// A filter that cannot answer is not a filter that hid nothing: the read
	// fails instead of serving what the caller could not decide about.
	if _, _, err := store.BlockServing(ctx, scope, "conv-1",
		func(string) (bool, error) { return false, errors.New("overlay unreadable") }); err == nil {
		t.Fatal("an unreadable filter served the block")
	}
}

// TestBlockIgnoresAJudgmentAnotherPolicyWrote pins the policy gate: the pass's
// half is served only under the vocabulary and bound that judged it.
func TestBlockIgnoresAJudgmentAnotherPolicyWrote(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	scope := testScope()
	judging := newTestStore(t, kv, Config{Distill: true})
	if _, err := judging.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor."}); err != nil {
		t.Fatal(err)
	}
	entries := Distill(
		[]factview.Fact{fact("fact-1", "Melanie loves pottery.")},
		nil, judging.Config())
	if _, err := judging.SaveDistilled(ctx, scope, "conv-1", entries, testNow); err != nil {
		t.Fatal(err)
	}

	// A store serving another vocabulary does not serve the earlier pass's
	// lines: they were judged by a rule it does not serve.
	other := newTestStore(t, kv, Config{Distill: true, Classes: []Class{{Name: "role", Phrases: []string{"is a counselor"}}}})
	block, found, err := other.Block(ctx, scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || block.Entries != 1 || block.Text != "- Alice is a counselor." {
		t.Fatalf("block under another policy = %+v (found=%t), want the host's line alone", block, found)
	}

	// Turning distillation off hides the pass's half the same way, and its own
	// read still reports both halves so a host can see what is stored.
	off := newTestStore(t, kv, Config{})
	block, found, err = off.Block(ctx, scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || block.Entries != 1 {
		t.Fatalf("block with distillation off = %+v (found=%t), want the host's line alone", block, found)
	}
	document, found, err := off.Load(ctx, scope, "conv-1")
	if err != nil || !found {
		t.Fatalf("Load = found=%t err=%v", found, err)
	}
	if len(document.Distilled) != 1 {
		t.Fatalf("stored distilled half = %+v, want the stored judgment kept", document.Distilled)
	}
}

// TestSaveRefusesAHostWriteThatDoesNotFitTheBound pins all-or-nothing: a write
// the block cannot hold reports an error and leaves the block as it was.
func TestSaveRefusesAHostWriteThatDoesNotFitTheBound(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	scope := testScope()
	small := newTestStore(t, kv, Config{MaxTokens: 8})
	if _, err := small.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor who works with families."}); err == nil {
		t.Fatal("SaveExplicit accepted a line over the token bound")
	}
	if _, found, err := small.Load(ctx, scope, "conv-1"); err != nil || found {
		t.Fatalf("refused write left a block behind: found=%t err=%v", found, err)
	}

	counted := newTestStore(t, kv, Config{MaxEntries: 2})
	if _, err := counted.SaveExplicit(ctx, scope, "conv-2", []string{"Alice is a counselor.", "Melanie is a painter.", "Tim is a writer."}); err == nil {
		t.Fatal("SaveExplicit accepted more entries than the block holds")
	}
	for _, texts := range [][]string{{""}, {"Alice is a counselor.", "Alice is a counselor."}} {
		if _, err := counted.SaveExplicit(ctx, scope, "conv-3", texts); err == nil {
			t.Fatalf("SaveExplicit accepted %q", texts)
		}
	}
	if _, found, err := counted.Load(ctx, scope, "conv-3"); err != nil || found {
		t.Fatalf("refused write left a block behind: found=%t err=%v", found, err)
	}
}

// TestSaveMakesRoomForTheHostsLines pins the two writers' precedence: the
// host's write lands even when the pass filled the block, dropping the pass's
// lowest-ranked lines and reporting how many.
func TestSaveMakesRoomForTheHostsLines(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{Distill: true, MaxTokens: 24})
	scope := testScope()
	entries := Distill([]factview.Fact{
		fact("fact-1", "Melanie loves pottery."),
		fact("fact-2", "Melanie loves live music."),
		fact("fact-3", "Melanie loves camping trips."),
	}, nil, store.Config())
	if len(entries) != 3 {
		t.Fatalf("Distill = %+v, want three lines", entries)
	}
	written, err := store.SaveDistilled(ctx, scope, "conv-1", entries, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Document.Distilled) != 3 {
		t.Fatalf("stored half = %+v, want three lines", written.Document.Distilled)
	}

	result, err := store.SaveExplicit(ctx, scope, "conv-1", []string{"Melanie is an artist."})
	if err != nil {
		t.Fatal(err)
	}
	if result.Dropped == 0 {
		t.Fatal("host write did not report making room")
	}
	if len(result.Document.Distilled) != 2 || len(result.Document.Explicit) != 1 {
		t.Fatalf("document = %+v, want the host's line and two distilled lines", result.Document)
	}
	block, found, err := store.Block(ctx, scope, "conv-1")
	if err != nil || !found {
		t.Fatalf("Block = found=%t err=%v", found, err)
	}
	if block.Entries != 3 || !strings.HasPrefix(block.Text, "- Melanie is an artist.") {
		t.Fatalf("block = %+v, want the host's line first", block)
	}
	if got := entriesTokens(append(append([]Entry(nil), result.Document.Explicit...), result.Document.Distilled...)); got > 24 {
		t.Fatalf("block costs %d tokens over its 24-token bound", got)
	}
}

// TestSaveDistilledConversationsValidatesBeforeWriting pins the batch shape: a
// call naming one block the store cannot hold writes none of them.
func TestSaveDistilledConversationsValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{Distill: true})
	scope := testScope()
	good := Distill([]factview.Fact{fact("fact-1", "Melanie loves pottery.")}, nil, store.Config())
	bad := append([]Entry(nil), good...)
	bad[0].Text = ""
	if _, err := store.SaveDistilledConversations(ctx, scope, map[string][]Entry{"conv-1": good, "conv-2": bad}, testNow); err == nil {
		t.Fatal("SaveDistilledConversations accepted an empty line")
	}
	conversations, err := store.Conversations(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 0 {
		t.Fatalf("refused batch left blocks behind: %v", conversations)
	}
}

// TestStoredBlocksAreReadStrictly pins fail-closed reads: a document this
// version does not know, or one whose entry id and text disagree, is an error
// rather than a block served half-read.
func TestStoredBlocksAreReadStrictly(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{})
	scope := testScope()
	key, err := store.key(scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	unknown := Document{SchemaVersion: schemaVersion + 1}
	encoded, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, encoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Block(ctx, scope, "conv-1"); err == nil {
		t.Fatal("Block served a document written by another schema version")
	}
	tampered := Document{
		SchemaVersion: schemaVersion,
		Explicit:      []Entry{{ID: EntryID("Alice is a counselor."), Text: "Alice is a painter.", Origin: OriginExplicit}},
	}
	encoded, err = json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, encoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Block(ctx, scope, "conv-1"); err == nil {
		t.Fatal("Block served an entry whose id and text disagree")
	}
	if err := store.Clear(ctx, scope, "conv-1"); err != nil {
		t.Fatal(err)
	}
}

// TestBlockTruncatesADocumentStoredUnderALargerCap pins the defensive read: a
// block written under a larger cap is served cut to this store's bound, and the
// truncation is reported rather than hidden.
func TestBlockTruncatesADocumentStoredUnderALargerCap(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	scope := testScope()
	wide := newTestStore(t, kv, Config{MaxTokens: 200})
	if _, err := wide.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor.", "Melanie is a painter."}); err != nil {
		t.Fatal(err)
	}
	narrow := newTestStore(t, kv, Config{MaxTokens: 8})
	block, found, err := narrow.Block(ctx, scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !block.Truncated || block.Entries != 1 {
		t.Fatalf("block = %+v (found=%t), want one entry and a reported truncation", block, found)
	}
}

// TestClearRemovesTheBlockAndItsKey pins that clearing a block stops the
// conversation from being listed as one that has one.
func TestClearRemovesTheBlockAndItsKey(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{})
	scope := testScope()
	for _, conversationID := range []string{"conv-1", "conv-2"} {
		if _, err := store.SaveExplicit(ctx, scope, conversationID, []string{"Alice is a counselor " + conversationID + "."}); err != nil {
			t.Fatal(err)
		}
	}
	conversations, err := store.Conversations(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(conversations, ",") != "conv-1,conv-2" {
		t.Fatalf("Conversations = %v", conversations)
	}
	if err := store.Clear(ctx, scope, "conv-1"); err != nil {
		t.Fatal(err)
	}
	if conversations, err = store.Conversations(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if strings.Join(conversations, ",") != "conv-2" {
		t.Fatalf("Conversations after Clear = %v", conversations)
	}
	report, err := store.Diagnostics(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Blocks != 1 || report.Entries != 1 || report.Tokens == 0 {
		t.Fatalf("Diagnostics = %+v", report)
	}
	if report.MaxTokens != DefaultMaxTokens || report.MaxEntries != DefaultMaxEntries {
		t.Fatalf("Diagnostics bound = %+v, want the defaults", report)
	}
}

// TestConversationsFallsBackToAListingScan pins the substrate fallback: a store
// without the key-only scan lists the same conversations.
func TestConversationsFallsBackToAListingScan(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	swapper, putOnce := kv.(storage.CASStore), kv.(storage.PutIfAbsentStore)
	store, err := NewStore(keyLessKV{kv, swapper, putOnce},
		WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	if _, err := store.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor."}); err != nil {
		t.Fatal(err)
	}
	conversations, err := store.Conversations(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(conversations, ",") != "conv-1" {
		t.Fatalf("Conversations = %v", conversations)
	}
}

// TestStoreRequiresTheSwapExtensions pins the substrate contract: a KV without
// compare-and-swap cannot host a block two writers share.
func TestStoreRequiresTheSwapExtensions(t *testing.T) {
	kv := testKV(t)
	if _, err := NewStore(listOnlyKV{kv}); err == nil {
		t.Fatal("NewStore accepted a KV without compare-and-swap")
	}
}

// TestConcurrentWritersKeepBothHalves pins what the swap is for: a host write
// and a pass write landing together do not overwrite each other.
func TestConcurrentWritersKeepBothHalves(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	store := newTestStore(t, kv, Config{Distill: true})
	scope := testScope()
	entries := Distill([]factview.Fact{fact("fact-1", "Melanie loves pottery.")}, nil, store.Config())

	var wait sync.WaitGroup
	failures := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		if _, err := store.SaveExplicit(ctx, scope, "conv-1", []string{"Alice is a counselor."}); err != nil {
			failures <- err
		}
	}()
	go func() {
		defer wait.Done()
		if _, err := store.SaveDistilled(ctx, scope, "conv-1", entries, testNow); err != nil {
			failures <- err
		}
	}()
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	document, found, err := store.Load(ctx, scope, "conv-1")
	if err != nil || !found {
		t.Fatalf("Load = found=%t err=%v", found, err)
	}
	if len(document.Explicit) != 1 || len(document.Distilled) != 1 {
		t.Fatalf("document = %+v, want both writes kept", document)
	}
}
