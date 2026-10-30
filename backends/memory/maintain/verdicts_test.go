package maintain

import (
	"context"
	"strings"
	"testing"

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
	if err := store.SaveVerdicts(ctx, verdicts); err != nil {
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
	if err := store.SaveVerdicts(ctx, verdicts); err != nil {
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
	left, right := collidingVerdictPairs(t, 2)
	first.Remember(left, Judgment{Verdict: VerdictBothCurrent, Confidence: 0.7})
	if err := store.SaveVerdicts(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := store.Verdicts(ctx, scope, "arbiter")
	if err != nil {
		t.Fatal(err)
	}
	second.Remember(right, Judgment{Verdict: VerdictReplaced, Confidence: 0.9})
	if err := store.SaveVerdicts(ctx, second); err != nil {
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
	evictVerdicts(entries, 2)
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
	evictVerdicts(small, 2)
	if len(small) != 1 {
		t.Fatalf("entries = %#v", small)
	}
}

// collidingVerdictPairs returns pairs whose digests share one shard, which is
// what a merge test needs: two saves that touch the same document.
func collidingVerdictPairs(t *testing.T, count int) (Pair, Pair) {
	t.Helper()
	seen := map[string]Pair{}
	for index := 0; index < 2048; index++ {
		pair := verdictPair("older-"+strings.Repeat("x", index), "newer-"+strings.Repeat("y", index))
		shard := verdictDigest(pair)[:shardWidth]
		if other, found := seen[shard]; found {
			if count == 2 {
				return other, pair
			}
		}
		seen[shard] = pair
	}
	t.Fatal("no two pairs shared a shard")
	return Pair{}, Pair{}
}
