package maintain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func verdictPair(older, newer string) Pair {
	return Pair{
		Conversation: "conv-1",
		Older:        factview.Fact{ID: "old", Text: older},
		Newer:        factview.Fact{ID: "new", Text: newer},
	}
}

func testVerdicts(t *testing.T, identity string) (*Store, corememory.Scope, *Verdicts) {
	t.Helper()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	verdicts, err := store.Verdicts(context.Background(), scope, identity)
	if err != nil {
		t.Fatal(err)
	}
	return store, scope, verdicts
}

// testVerdictsAt is testVerdicts with the store's backing store handed back, so
// a test can put a second store -- a later pass -- over the same documents.
func testVerdictsAt(t *testing.T, identity string) (*Store, storage.Store, corememory.Scope, *Verdicts) {
	t.Helper()
	store, kv := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	verdicts, err := store.Verdicts(context.Background(), scope, identity)
	if err != nil {
		t.Fatal(err)
	}
	return store, kv, scope, verdicts
}

// TestVerdictsRoundTripRemembersTheAnswer pins the cache's one promise: a pair
// an earlier pass paid for is answered without a call, and the answer it
// returns is the one that was given -- verdict and probability.
func TestVerdictsRoundTripRemembersTheAnswer(t *testing.T) {
	ctx := context.Background()
	store, scope, verdicts := testVerdicts(t, "deepseek/azure-decision@supersede-v1,p=0.75")
	pair := verdictPair("郑凯住在柏林。", "郑凯搬去了巴黎。")
	if _, remembered := verdicts.Lookup(pair); remembered {
		t.Fatal("an empty scope remembered an answer")
	}
	verdicts.Remember(pair, Judgment{Verdict: VerdictBothCurrent, Confidence: 0.82})
	if verdicts.Pending() != 1 {
		t.Fatalf("pending = %d", verdicts.Pending())
	}
	if _, err := store.SaveVerdicts(ctx, verdicts, DefaultArbiterRemember); err != nil {
		t.Fatal(err)
	}
	// A second pass over the same scope reads what the first one paid for.
	next, err := store.Verdicts(ctx, scope, "deepseek/azure-decision@supersede-v1,p=0.75")
	if err != nil {
		t.Fatal(err)
	}
	judgment, remembered := next.Lookup(pair)
	if !remembered {
		t.Fatal("the saved answer was not read back")
	}
	if judgment.Verdict != VerdictBothCurrent || judgment.Confidence != 0.82 {
		t.Fatalf("judgment = %+v", judgment)
	}
}

// TestVerdictsAreScopedToTheArbiterThatGaveThem pins the invalidation: the
// threshold gates findings, so "replaced" at 0.75 is not an answer at 0.9, and
// a shard one arbiter wrote is not read under another's name.
func TestVerdictsAreScopedToTheArbiterThatGaveThem(t *testing.T) {
	ctx := context.Background()
	store, scope, verdicts := testVerdicts(t, "deepseek/azure-decision@supersede-v1,p=0.75")
	pair := verdictPair("a", "b")
	verdicts.Remember(pair, Judgment{Verdict: VerdictReplaced, Confidence: 0.8})
	if _, err := store.SaveVerdicts(ctx, verdicts, DefaultArbiterRemember); err != nil {
		t.Fatal(err)
	}
	stricter, err := store.Verdicts(ctx, scope, "deepseek/azure-decision@supersede-v1,p=0.9")
	if err != nil {
		t.Fatal(err)
	}
	if _, remembered := stricter.Lookup(pair); remembered {
		t.Fatal("a verdict from another threshold was served")
	}
}

// TestVerdictDigestNamesTheQuestion pins what makes a remembered answer safe to
// reuse: the digest is the statements the arbiter read, not the ids the store
// gave them. A rebuilt fact with the same text is the same question; a fact
// whose stated time changed is a different one.
func TestVerdictDigestNamesTheQuestion(t *testing.T) {
	first := Pair{
		Older: factview.Fact{ID: "fact-1", Text: "郑凯住在柏林。"},
		Newer: factview.Fact{ID: "fact-2", Text: "郑凯搬去了巴黎。"},
	}
	// Same text under ids a rebuild assigned: the same question.
	rebuilt := first
	rebuilt.Older.ID, rebuilt.Newer.ID = "fact-9", "fact-8"
	if verdictDigest(first) != verdictDigest(rebuilt) {
		t.Fatal("a rebuilt id changed the digest")
	}
	// A stated time is part of the question the arbiter reads.
	dated := rebuilt
	dated.Newer.TemporalDetail = "上个月"
	if verdictDigest(first) == verdictDigest(dated) {
		t.Fatal("a stated time did not change the digest")
	}
	// The pair is ordered, so the two facts cannot digest the way the
	// reverse pair would.
	reversed := Pair{Older: first.Newer, Newer: first.Older}
	if verdictDigest(first) == verdictDigest(reversed) {
		t.Fatal("the reverse pair digested the same")
	}
}

// TestSaveVerdictsMergesIntoTheShard pins the partial pass: a pass that judged
// one conversation writes the answers it has and keeps the ones another pass
// already stored in the same shard.
func TestSaveVerdictsMergesIntoTheShard(t *testing.T) {
	ctx := context.Background()
	store, scope, first := testVerdicts(t, "arbiter")
	// Two pairs whose digests land in one shard, so the second save has to
	// read what the first one wrote.
	colliding := collidingVerdictPairs(t, 2)
	left, right := colliding[0], colliding[1]
	first.Remember(left, Judgment{Verdict: VerdictBothCurrent, Confidence: 0.7})
	if _, err := store.SaveVerdicts(ctx, first, DefaultArbiterRemember); err != nil {
		t.Fatal(err)
	}
	second, err := store.Verdicts(ctx, scope, "arbiter")
	if err != nil {
		t.Fatal(err)
	}
	second.Remember(right, Judgment{Verdict: VerdictReplaced, Confidence: 0.9})
	if _, err := store.SaveVerdicts(ctx, second, DefaultArbiterRemember); err != nil {
		t.Fatal(err)
	}
	read, err := store.Verdicts(ctx, scope, "arbiter")
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]Pair{"first": left, "second": right} {
		if _, remembered := read.Lookup(pair); !remembered {
			t.Errorf("the %s pass's answer was dropped by the second save", name)
		}
	}
}

// TestEvictVerdictsKeepsTheNewest pins the bound: a full shard forgets its
// oldest answers, and the order it forgets them in does not depend on map
// iteration.
func TestEvictVerdictsKeepsTheNewest(t *testing.T) {
	entries := map[string]verdictEntry{
		"aaa": {Verdict: VerdictBothCurrent, Seen: 10},
		"bbb": {Verdict: VerdictBothCurrent, Seen: 30},
		"ccc": {Verdict: VerdictBothCurrent, Seen: 20},
		"ddd": {Verdict: VerdictBothCurrent, Seen: 40},
	}
	if dropped := evictVerdicts(entries, 2); dropped != 2 {
		t.Fatalf("dropped = %d", dropped)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	for _, kept := range []string{"bbb", "ddd"} {
		if _, found := entries[kept]; !found {
			t.Errorf("the newest answer %q was evicted", kept)
		}
	}
	// Under the limit nothing is forgotten.
	small := map[string]verdictEntry{"only": {Verdict: VerdictBothCurrent}}
	if dropped := evictVerdicts(small, 2); dropped != 0 {
		t.Fatalf("dropped = %d", dropped)
	}
	if len(small) != 1 {
		t.Fatalf("entries = %#v", small)
	}
}

// TestVerdictCeilingRotatesAndSaysSo pins the scope ceiling: a pass that is
// handed more answers than the store keeps writes the newest ones, drops the
// oldest, and reports the count. The count is the reading a deployment acts on
// -- the same answers are bought again on the next pass -- so it must not be
// silent.
func TestVerdictCeilingRotatesAndSaysSo(t *testing.T) {
	ctx := context.Background()
	store, kv, scope, verdicts := testVerdictsAt(t, "arbiter")
	// Two answers a shard, so the third answer in the same shard is the one
	// that overflows the scope's store.
	remember := verdictShards * 2
	colliding := collidingVerdictPairs(t, 3)
	for _, pair := range colliding[:2] {
		verdicts.Remember(pair, Judgment{Verdict: VerdictBothCurrent, Confidence: 0.6})
	}
	if dropped, err := store.SaveVerdicts(ctx, verdicts, remember); err != nil || dropped != 0 {
		t.Fatalf("first save: dropped=%d err=%v", dropped, err)
	}
	// A later pass over the same scope asks one more pair: the store is full,
	// so the answer this pass paid for has to survive and an older one go.
	later, err := NewStore(kv, WithClock(func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	next, err := later.Verdicts(ctx, scope, "arbiter")
	if err != nil {
		t.Fatal(err)
	}
	next.Remember(colliding[2], Judgment{Verdict: VerdictBothCurrent, Confidence: 0.9})
	dropped, err := later.SaveVerdicts(ctx, next, remember)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	read, err := store.Verdicts(ctx, scope, "arbiter")
	if err != nil {
		t.Fatal(err)
	}
	// The newest answer survives, and exactly one of the two the earlier pass
	// paid for is what the ceiling cost.
	if _, remembered := read.Lookup(colliding[2]); !remembered {
		t.Error("the newest answer was evicted")
	}
	kept := 0
	for _, pair := range colliding[:2] {
		if _, remembered := read.Lookup(pair); remembered {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("kept %d of the older answers, want 1", kept)
	}
}

// TestVerdictCeilingRoundsUpPerShard pins the arithmetic between the configured
// answer count and what a shard holds: the scope keeps at least the count it was
// given, and a shard keeps at least one answer whatever the setting.
func TestVerdictCeilingRoundsUpPerShard(t *testing.T) {
	if got := verdictEntriesPerShard(0); got != DefaultArbiterRemember/verdictShards {
		t.Fatalf("default shard limit = %d", got)
	}
	if got := verdictEntriesPerShard(verdictShards*3 + 1); got != 4 {
		t.Fatalf("rounded shard limit = %d, want 4", got)
	}
	if got := verdictEntriesPerShard(1); got != 1 {
		t.Fatalf("floor shard limit = %d, want 1", got)
	}
}

// collidingVerdictPairs returns pairs whose digests share one shard, which is
// what a merge test needs: two saves that touch the same document.
func collidingVerdictPairs(t *testing.T, count int) []Pair {
	t.Helper()
	pairs := make([]Pair, 0, count)
	for index := 0; index < 2048; index++ {
		pair := verdictPair("older-"+strings.Repeat("x", index), "newer-"+strings.Repeat("y", index))
		if len(pairs) > 0 && verdictDigest(pair)[:shardWidth] != verdictDigest(pairs[0])[:shardWidth] {
			continue
		}
		pairs = append(pairs, pair)
		if len(pairs) == count {
			return pairs
		}
	}
	t.Fatalf("found %d pairs in one shard, want %d", len(pairs), count)
	return nil
}
