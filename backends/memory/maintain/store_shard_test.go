package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// countingStore counts the value bytes a read pulls out of the substrate: that
// is what an overlay read costs the request that caused it, whatever the
// underlying store caches or streams.
type countingStore struct {
	storage.Store
	valueBytes int
	gets       int
	lists      int
}

func (store *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := store.Store.Get(ctx, key)
	store.gets++
	if err == nil {
		store.valueBytes += len(data)
	}
	return data, err
}

func (store *countingStore) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	entries, err := store.Store.List(ctx, prefix)
	store.lists++
	if err != nil {
		return entries, err
	}
	for _, entry := range entries {
		store.valueBytes += len(entry.Value)
	}
	return entries, nil
}

func (store *countingStore) reset() {
	store.valueBytes, store.gets, store.lists = 0, 0, 0
}

// TestSaveConversationsReplacesOnlyTheNamedConversations pins the write shape
// an incremental pass needs: the named conversations are replaced with what the
// pass judged -- including a conversation it judged and found nothing in -- and
// the conversations it was not given keep the factors an earlier pass wrote.
func TestSaveConversationsReplacesOnlyTheNamedConversations(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := store.Save(ctx, Plan{Scope: scope, Decays: []Decay{
		{FactID: "stale-a", Conversation: "conv-a", Score: 0.3},
		{FactID: "stale-b", Conversation: "conv-b", Score: 0.4},
		{FactID: "stale-c", Conversation: "conv-c", Score: 0.5},
	}}); err != nil {
		t.Fatal(err)
	}

	// The incremental pass judged conv-b alone: its findings replace the
	// factors the full pass wrote for that conversation.
	if err := store.SaveConversations(ctx, Plan{Scope: scope,
		Supersedes: []Supersede{{FactID: "stale-b", Conversation: "conv-b", SupersededBy: "fresh-b", Similarity: 0.7}},
		Decays:     []Decay{{FactID: "fresh-b", Conversation: "conv-b", Score: 0.6}},
	}, []string{"conv-b"}); err != nil {
		t.Fatal(err)
	}

	overlay, err := store.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 4 {
		t.Fatalf("overlay after the partial save = %#v", overlay)
	}
	for conversation, factor := range map[string]float64{"conv-a": 0.3, "conv-c": 0.5} {
		key, err := itemKey(scope, conversation, "stale-"+conversation[5:])
		if err != nil {
			t.Fatal(err)
		}
		if entry := overlay[key]; entry.Factor != factor {
			t.Fatalf("%s: entry = %+v, want factor %v", conversation, entry, factor)
		}
	}
	superseded, err := itemKey(scope, "conv-b", "stale-b")
	if err != nil {
		t.Fatal(err)
	}
	if entry := overlay[superseded]; entry.SupersededBy != "fresh-b" || entry.Factor != defaultSupersedeFactor {
		t.Fatalf("conv-b's replaced entry = %+v", entry)
	}
	decayed, err := itemKey(scope, "conv-b", "fresh-b")
	if err != nil {
		t.Fatal(err)
	}
	if entry := overlay[decayed]; entry.Factor != 0.6 {
		t.Fatalf("conv-b's new entry = %+v", entry)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.Entries != 4 {
		t.Fatalf("status after the partial save = %+v", status)
	}

	// A pass that judged a conversation and found nothing in it drops that
	// conversation's entries: they are exactly the factors it just declared
	// absent.
	if err := store.SaveConversations(ctx, Plan{Scope: scope}, []string{"conv-a"}); err != nil {
		t.Fatal(err)
	}
	if overlay, err := store.Load(ctx, scope); err != nil || len(overlay) != 3 {
		t.Fatalf("overlay after judging conv-a empty = %#v, %v", overlay, err)
	}
}

// TestPartialSaveKeepsTheFullPassStamp pins what the expiry signal measures: a
// partial save moves the entries it replaced and not the time of the scope's
// last full pass, so a scope maintained conversation by conversation still
// reports the full pass it is waiting for.
func TestPartialSaveKeepsTheFullPassStamp(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(kv, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.Save(ctx, Plan{Scope: scope, Decays: []Decay{
		{FactID: "stale-a", Conversation: "conv-a", Score: 0.3},
	}}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(48 * time.Hour)
	if err := store.SaveConversations(ctx, Plan{Scope: scope,
		Decays: []Decay{{FactID: "stale-b", Conversation: "conv-b", Score: 0.4}},
	}, []string{"conv-b"}); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if status.Entries != 2 {
		t.Fatalf("status counts = %+v", status)
	}
	if want := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC); !status.UpdatedAt.Equal(want) {
		t.Fatalf("partial save moved the full-pass stamp: %+v", status)
	}

	// One half-life after the full pass the scope reports the pass it is
	// waiting for, however recently a conversation was judged.
	now = now.Add(720 * time.Hour)
	status, err = store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Expired || status.Stale {
		t.Fatalf("status one half-life after the full pass = %+v", status)
	}
}

// shardFixture is a scope's worth of conversations, each holding a supersede
// pair, a decayed fact, and a fact sharing neither text nor entity with the
// others: enough shape that a whole pass and a conversation-at-a-time pass
// have something to disagree about if they ever do.
func shardFixture(now time.Time) map[string][]factview.Fact {
	conversations := map[string][]factview.Fact{}
	for index, conversation := range []string{"conv-a", "conv-b", "conv-c", "conv-d"} {
		base := now.Add(-time.Duration(index+1) * time.Hour)
		conversations[conversation] = []factview.Fact{
			shardFact(conversation, "stale", "Alice works at Acme Corporation", []string{"Alice", "Acme"},
				now.Add(-1250*time.Hour)),
			shardFact(conversation, "claim", "Alice lives in Berlin", []string{"Alice"}, base),
			shardFact(conversation, "restated", "Alice lives in Berlin now", []string{"Alice"}, base.Add(time.Hour)),
			shardFact(conversation, "tangent", "Bob likes tea", []string{"Bob"}, base.Add(2*time.Hour)),
		}
	}
	return conversations
}

func shardFact(conversation, id, text string, entities []string, at time.Time) factview.Fact {
	fact := planFact(id, text, entities, at)
	fact.ConversationID = conversation
	return fact
}

// TestSaveConversationsMatchesAWholeSave is the equivalence the partial write
// owes the read path: a scope built conversation by conversation from a full
// pass seed reads back exactly like the same findings written in one save.
func TestSaveConversationsMatchesAWholeSave(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	conversations := shardFixture(now)

	whole, _ := testStore(t)
	fullPlan, err := Detect(scope, conversations, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullPlan.Supersedes) < 4 || len(fullPlan.Decays) < 4 {
		t.Fatalf("fixture found nothing to compare: %+v", fullPlan)
	}
	if err := whole.Save(ctx, fullPlan); err != nil {
		t.Fatal(err)
	}

	incremental, _ := testStore(t)
	// The full pass a deployment runs once: it judged nothing yet, which is
	// the state a partial save needs to build on.
	if err := incremental.Save(ctx, Plan{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	for _, conversation := range sortedKeys(conversations) {
		plan, err := Detect(scope, map[string][]factview.Fact{conversation: conversations[conversation]}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := incremental.SaveConversations(ctx, plan, []string{conversation}); err != nil {
			t.Fatal(err)
		}
	}

	wholesome, err := whole.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	sharded, err := incremental.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharded) != len(wholesome) || len(wholesome) == 0 {
		t.Fatalf("sharded overlay has %d entries, the whole save %d", len(sharded), len(wholesome))
	}
	for identity, entry := range wholesome {
		other, ok := sharded[identity]
		if !ok || other.Factor != entry.Factor || other.SupersededBy != entry.SupersededBy {
			t.Fatalf("entry %s: partial save wrote %+v, whole save %+v", identity, other, entry)
		}
	}
	wholeScores, err := whole.ScoreOverlay(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	shardedScores, err := incremental.ScoreOverlay(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(wholeScores) != len(shardedScores) {
		t.Fatalf("scores differ: %d vs %d entries", len(shardedScores), len(wholeScores))
	}
	for identity, factor := range wholeScores {
		if shardedScores[identity] != factor {
			t.Fatalf("score %s = %v, want %v", identity, shardedScores[identity], factor)
		}
	}
	wholeStatus, err := whole.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	shardedStatus, err := incremental.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if shardedStatus.Entries != wholeStatus.Entries || shardedStatus.Stale != wholeStatus.Stale {
		t.Fatalf("status differs: %+v vs %+v", shardedStatus, wholeStatus)
	}
}

// TestSaveConversationsRefusesCallsItCannotHonor pins the guards: a partial
// save has no state to keep without this pass's control record, it refuses a
// plan whose findings it would silently drop, and it never adopts an overlay
// another pass wrote under its own stamp.
func TestSaveConversationsRefusesCallsItCannotHonor(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	plan := Plan{Scope: scope, Decays: []Decay{{FactID: "stale-a", Conversation: "conv-a", Score: 0.3}}}
	if err := store.SaveConversations(ctx, plan, []string{"conv-a"}); err == nil ||
		!strings.Contains(err.Error(), "full save") {
		t.Fatalf("partial save without an overlay = %v", err)
	}
	if err := store.Save(ctx, Plan{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversations(ctx, plan, []string{"  "}); err == nil ||
		!strings.Contains(err.Error(), "conversation") {
		t.Fatalf("partial save without a conversation = %v", err)
	}
	wide := Plan{Scope: scope, Decays: []Decay{
		{FactID: "stale-a", Conversation: "conv-a", Score: 0.3},
		{FactID: "stale-b", Conversation: "conv-b", Score: 0.4},
	}}
	if err := store.SaveConversations(ctx, wide, []string{"conv-a"}); err == nil ||
		!strings.Contains(err.Error(), "conv-b") {
		t.Fatalf("partial save that dropped a judgment = %v", err)
	}

	foreign, err := NewStore(kv, WithConfig(Config{SimilarityThreshold: 0.9}))
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Save(ctx, Plan{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversations(ctx, plan, []string{"conv-a"}); err == nil ||
		!strings.Contains(err.Error(), "this store serves") {
		t.Fatalf("partial save into another pass's overlay = %v", err)
	}
}

// TestStoreReportsAndClearsThePreShardOverlay pins the upgrade path: the one
// document an older layout left is counted and reported as stale -- nothing
// reads it -- and the next save replaces the scope's state with the sharded
// layout and clears the key.
func TestStoreReportsAndClearsThePreShardOverlay(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	identity, err := itemKey(scope, "conv-1", "stale")
	if err != nil {
		t.Fatal(err)
	}
	const producer = "maintain-v3+similarity=0.5,supersede=0.5,half_life=720h0m0s,floor=0.5,recall_saturation=8"
	judged := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key, err := legacyOverlayKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"producer_version": producer,
		"entries":          map[string]Overlay{identity: {Factor: 0.3, UpdatedAt: judged}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}

	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || !status.Stale || status.Entries != 1 || status.StoredProducer != producer {
		t.Fatalf("status of the pre-shard document = %+v", status)
	}
	if !status.UpdatedAt.Equal(judged) || status.Expired {
		t.Fatalf("stale document reported fresh or expired: %+v", status)
	}
	if overlay, err := store.Load(ctx, scope); err != nil || len(overlay) != 0 {
		t.Fatalf("pre-shard overlay was read = %#v, %v", overlay, err)
	}
	if scores, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-1"}); err != nil || len(scores) != 0 {
		t.Fatalf("scoped read of a pre-shard overlay = %#v, %v", scores, err)
	}
	if factor, err := store.Adjust(ctx, scope, identity); err != nil || factor != 1 {
		t.Fatalf("adjustment from a pre-shard overlay = %v, %v", factor, err)
	}

	if err := store.Save(ctx, Plan{Scope: scope,
		Decays: []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.25}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pre-shard document survived the save: %v", err)
	}
	status, err = store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.Entries != 1 || status.StoredProducer != status.ExpectedProducer {
		t.Fatalf("status after the upgrade pass = %+v", status)
	}
	scores, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-1"})
	if err != nil || len(scores) != 1 || scores[identity] != 0.25 {
		t.Fatalf("scoped read after the upgrade pass = %#v, %v", scores, err)
	}
}

// TestScoreOverlayForMatchesTheBulkRead pins the scoped read's equivalence: a
// request that names its candidates' conversations gets exactly the factors
// the whole-scope read would have given it for them, and a conversation no
// pass judged is empty rather than an error.
func TestScoreOverlayForMatchesTheBulkRead(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := store.Save(ctx, Plan{Scope: scope,
		Supersedes: []Supersede{{FactID: "old-a", Conversation: "conv-a", SupersededBy: "new-a", Similarity: 0.6}},
		Decays: []Decay{
			{FactID: "stale-a", Conversation: "conv-a", Score: 0.3},
			{FactID: "stale-b", Conversation: "conv-b", Score: 0.4},
			{FactID: "stale-c", Conversation: "conv-c", Score: 0.2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{}
	for conversation, factID := range map[string]string{"conv-a": "stale-a", "conv-c": "stale-c"} {
		identity, err := itemKey(scope, conversation, factID)
		if err != nil {
			t.Fatal(err)
		}
		want[identity] = map[string]float64{"conv-a": 0.3, "conv-c": 0.2}[conversation]
	}
	superseded, err := itemKey(scope, "conv-a", "old-a")
	if err != nil {
		t.Fatal(err)
	}
	want[superseded] = defaultSupersedeFactor

	// Repeats are the caller's normal shape: one entry per candidate.
	scores, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-a", "conv-c", "conv-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != len(want) {
		t.Fatalf("scoped read = %#v, want %#v", scores, want)
	}
	for identity, factor := range want {
		if scores[identity] != factor {
			t.Fatalf("scoped read %s = %v, want %v", identity, scores[identity], factor)
		}
	}
	if scores, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-none"}); err != nil || len(scores) != 0 {
		t.Fatalf("scoped read of an unjudged conversation = %#v, %v", scores, err)
	}
}

// TestOverlayReadGrowsWithCandidatesNotTheScope is the read-cost acceptance of
// the shard: what a request reads is the conversations it names, so growing the
// scope leaves the request's bytes where they were.
func TestOverlayReadGrowsWithCandidatesNotTheScope(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingStore{Store: kv}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(counting, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	plan := func(count int) Plan {
		judged := Plan{Scope: scope}
		for conversation := 0; conversation < count; conversation++ {
			for entry := 0; entry < 20; entry++ {
				judged.Decays = append(judged.Decays, Decay{
					FactID:       fmt.Sprintf("fact-%d-%d", conversation, entry),
					Conversation: fmt.Sprintf("conv-%02d", conversation),
					Score:        0.3,
				})
			}
		}
		return judged
	}
	if err := store.Save(ctx, plan(12)); err != nil {
		t.Fatal(err)
	}

	counting.reset()
	scores, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-00"})
	if err != nil || len(scores) != 20 {
		t.Fatalf("scoped read = %d scores, %v", len(scores), err)
	}
	scoped := counting.valueBytes
	counting.reset()
	whole, err := store.ScoreOverlay(ctx, scope)
	if err != nil || len(whole) != 240 {
		t.Fatalf("bulk read = %d scores, %v", len(whole), err)
	}
	bulk := counting.valueBytes
	if scoped <= 0 || bulk <= scoped {
		t.Fatalf("scoped read %d bytes, bulk read %d", scoped, bulk)
	}
	if scoped > bulk/4 {
		t.Fatalf("one conversation of twelve cost %d bytes of the scope's %d", scoped, bulk)
	}
	t.Logf("overlay read: %d bytes for one conversation of twelve, %d bytes for the scope (240 entries)",
		scoped, bulk)

	// Four times the scope, the same request: the bytes are the conversation's
	// own, and only the scope-wide read grows.
	if err := store.Save(ctx, plan(48)); err != nil {
		t.Fatal(err)
	}
	counting.reset()
	if _, err := store.ScoreOverlayFor(ctx, scope, []string{"conv-00"}); err != nil {
		t.Fatal(err)
	}
	scaled := counting.valueBytes
	if scaled != scoped {
		t.Fatalf("scoped read after growing the scope = %d bytes, want %d", scaled, scoped)
	}
	counting.reset()
	if _, err := store.ScoreOverlay(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if grown := counting.valueBytes; grown <= bulk {
		t.Fatalf("bulk read after growing the scope = %d bytes, want more than %d", grown, bulk)
	}
	t.Logf("overlay read after four times the scope: %d bytes for the same conversation, %d bytes for the scope (960 entries)",
		scaled, counting.valueBytes)
}

// TestRunConversationsKeepsTheConversationsItWasNotGiven is the incremental
// pass end to end: the conversation the host named is re-judged against its
// current facts, and the conversation it did not name keeps the factors the
// last full pass wrote.
func TestRunConversationsKeepsTheConversationsItWasNotGiven(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	publish := func(conversation, id, text string, entities []string, at time.Time) {
		t.Helper()
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: id, Generation: "generation-1", Scope: scope, ConversationID: conversation,
			Content:    coremessage.NewTextContent(text),
			Entities:   entities,
			EventTime:  at,
			Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-" + conversation}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := facts.PublishActiveGeneration(ctx, scope, conversation, "generation-1"); err != nil {
			t.Fatal(err)
		}
	}
	publish("conv-a", "stale-a", "Alice works at Acme", []string{"Alice", "Acme"}, now.Add(-1250*time.Hour))
	publish("conv-b", "stale-b", "Bob works at Beta", []string{"Bob", "Beta"}, now.Add(-1250*time.Hour))

	service := &Service{Facts: facts, Store: store, Clock: func() time.Time { return now }}
	if _, err := service.RunScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	kept, err := itemKey(scope, "conv-b", "stale-b")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ScoreOverlay(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || before[kept] <= 0 {
		t.Fatalf("full pass wrote %#v", before)
	}

	// A fact lands in conv-a: the incremental pass re-judges that
	// conversation, where it finds the claim restated and the older decayed
	// entry superseded.
	publish("conv-a", "restated", "Alice works at Acme Corporation", []string{"Alice", "Acme"}, now.Add(-time.Hour))
	if _, err := service.RunConversations(ctx, scope, []string{"conv-a"}); err != nil {
		t.Fatal(err)
	}
	after, err := store.ScoreOverlay(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if after[kept] != before[kept] {
		t.Fatalf("conv-b's factor moved: %v -> %v", before[kept], after[kept])
	}
	staleA, err := itemKey(scope, "conv-a", "stale-a")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := store.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if entry := overlay[staleA]; entry.SupersededBy != "restated" {
		t.Fatalf("conv-a's re-judged entry = %+v", entry)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if status.Stale || status.Entries != 2 {
		t.Fatalf("status after the incremental pass = %+v", status)
	}
}

// TestRunConversationsRebuildsAScopeItCannotRead pins the fallback: a scope
// whose overlay belongs to another pass has no judgments to keep, so the
// incremental call is the whole pass, and it says so.
func TestRunConversationsRebuildsAScopeItCannotRead(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for _, conversation := range []string{"conv-a", "conv-b"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: "stale-" + conversation, Generation: "generation-1", Scope: scope, ConversationID: conversation,
			Content:    coremessage.NewTextContent("Alice works at Acme"),
			Entities:   []string{"Alice", "Acme"},
			EventTime:  now.Add(-1250 * time.Hour),
			Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := facts.PublishActiveGeneration(ctx, scope, conversation, "generation-1"); err != nil {
			t.Fatal(err)
		}
	}
	foreign, err := NewStore(kv, WithConfig(Config{SimilarityThreshold: 0.9}))
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Save(ctx, Plan{Scope: scope, Decays: []Decay{
		{FactID: "stale-conv-a", Conversation: "conv-a", Score: 0.3},
	}}); err != nil {
		t.Fatal(err)
	}

	service := &Service{Facts: facts, Store: store, Clock: func() time.Time { return now }}
	plan, err := service.RunConversations(ctx, scope, []string{"conv-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IgnoredOverlay {
		t.Fatalf("pass did not report the overlay it replaced: %+v", plan)
	}
	// The fallback judged the whole scope: conv-b's fact is in the plan too.
	judged := map[string]bool{}
	for _, decay := range plan.Decays {
		judged[decay.Conversation] = true
	}
	if !judged["conv-a"] || !judged["conv-b"] {
		t.Fatalf("fallback judged %#v, want the whole scope", plan.Decays)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.StoredProducer != status.ExpectedProducer {
		t.Fatalf("status after the fallback pass = %+v", status)
	}
	if _, err := service.RunConversations(ctx, scope, nil); err == nil ||
		!strings.Contains(err.Error(), "at least one conversation") {
		t.Fatalf("pass without a conversation = %v", err)
	}
}
