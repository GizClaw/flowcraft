package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRetrievalSummaryRendersTheResolvedSettings pins the fingerprint value the
// derivation policy digest does not cover: lane weights and fusion, the bm25
// parameters, source quotes and rerank, as the settings resolved them. The
// exact strings are the point -- a knob added to Lanes, BM25 or Retrieval
// changes this output, and this test then has to be updated on purpose.
func TestRetrievalSummaryRendersTheResolvedSettings(t *testing.T) {
	settings := Settings{}
	settings.applyDefaults()
	summary, err := settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"lanes":{"vector":{"weight":1},"bm25":{"weight":0.6},"entity":{"weight":0.4},"summary":{"weight":0.5},"fusion":"rrf"},"bm25":{},"retrieval":{}}`
	if summary != want {
		t.Fatalf("default summary = %s, want %s", summary, want)
	}

	settings.Lanes.Fusion = "weighted"
	settings.Lanes.Vector.Weight = 2
	settings.BM25.K1, settings.BM25.B = 1.4, 0.4
	settings.Retrieval.SourceQuotes, settings.Retrieval.Rerank = 3, true
	settings.Lanes.VectorCache = &VectorCacheSettings{MaxCachedScopes: 4, MaxEntriesPerScope: 20000}
	summary, err = settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lanes":{"vector":{"weight":2},"bm25":{"weight":0.6},"entity":{"weight":0.4},"summary":{"weight":0.5},"fusion":"weighted","vector_cache":{"max_cached_scopes":4,"max_entries_per_scope":20000}},"bm25":{"k1":1.4,"b":0.4},"retrieval":{"source_quotes":3,"rerank":true}}`
	if summary != want {
		t.Fatalf("tuned summary = %s, want %s", summary, want)
	}
}

// TestVectorCacheBoundsAreValidatedAndNamed pins the knob's shape: it is absent
// from the summary unless a host names it (so no existing run's fingerprint
// moves), and a count of what may be resident cannot be negative.
func TestVectorCacheBoundsAreValidatedAndNamed(t *testing.T) {
	settings := Settings{}
	settings.applyDefaults()
	if settings.Lanes.VectorCache != nil {
		t.Fatal("an untouched workspace names a vector cache bound")
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("an untouched workspace does not validate: %v", err)
	}
	for _, bound := range []VectorCacheSettings{
		{MaxCachedScopes: -1},
		{MaxEntriesPerScope: -1},
	} {
		invalid := Settings{}
		invalid.applyDefaults()
		invalid.Lanes.VectorCache = &bound
		if err := invalid.Validate(); err == nil {
			t.Fatalf("%+v was accepted", bound)
		}
	}
}

// TestObservedAtAnchorDefaultsOn pins the tri-state switch and the policy
// identity it carries: a document that does not name the key derives with the
// anchor, an explicit false opts out, and only the opt-out moves the derivation
// policy digest -- an absent key and an explicit true are one policy.
func TestObservedAtAnchorDefaultsOn(t *testing.T) {
	settingsWith := func(anchor *bool) Settings {
		settings := Settings{ObservedAtAnchor: anchor}
		settings.applyDefaults()
		return settings
	}
	on, off := true, false
	if !settingsWith(nil).anchorObservedAt() {
		t.Fatal("an absent observed_at_anchor must derive with the anchor")
	}
	if !settingsWith(&on).anchorObservedAt() {
		t.Fatal("an explicit true must derive with the anchor")
	}
	if settingsWith(&off).anchorObservedAt() {
		t.Fatal("an explicit false must opt out")
	}
	defaultDigest, err := policyDigest(settingsWith(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	onDigest, err := policyDigest(settingsWith(&on), "")
	if err != nil {
		t.Fatal(err)
	}
	if onDigest != defaultDigest {
		t.Fatal("the default and an explicit true must be one policy")
	}
	offDigest, err := policyDigest(settingsWith(&off), "")
	if err != nil {
		t.Fatal(err)
	}
	if offDigest == defaultDigest {
		t.Fatal("the opt-out must change the derivation policy digest")
	}
}

// TestAssemblyStampsTheRetrievalSummary wires the same value through the
// factory: what an assembly reports is the summary of the settings it was
// resolved with, so a retrieval block in a settings document reaches a host
// verbatim.
func TestAssemblyStampsTheRetrievalSummary(t *testing.T) {
	assembly, _ := newTestAssembly(t, `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "interval": "0",
  "lanes": {"fusion": "weighted", "vector": {"weight": 2}},
  "bm25": {"k1": 1.4, "b": 0.4},
  "retrieval": {"source_quotes": 3, "rerank": true}
}`)
	t.Cleanup(func() { _ = assembly.Close() })
	want := `{"lanes":{"vector":{"weight":2},"bm25":{"weight":0.6},"entity":{"weight":0.4},"summary":{"weight":0.5},"fusion":"weighted"},"bm25":{"k1":1.4,"b":0.4},"retrieval":{"source_quotes":3,"rerank":true}}`
	if got := assembly.RetrievalSummary(); got != want {
		t.Fatalf("RetrievalSummary() = %s, want %s", got, want)
	}
}

// TestFactQuotaMovesTheDerivationPolicyDigest pins the quota's identity. It
// changes what the extractor writes, so switching it on re-derives the scope --
// which is what the digest does -- while a workspace that leaves it off derives
// under the policy it always had: the off document carries no key, so the digest
// of an untouched workspace does not move.
func TestFactQuotaMovesTheDerivationPolicyDigest(t *testing.T) {
	withQuota := func(quota bool) Settings {
		settings := Settings{}
		settings.applyDefaults()
		settings.Fact.Quota = quota
		return settings
	}
	offDigest, err := policyDigest(withQuota(false), "")
	if err != nil {
		t.Fatal(err)
	}
	onDigest, err := policyDigest(withQuota(true), "")
	if err != nil {
		t.Fatal(err)
	}
	if offDigest == onDigest {
		t.Fatal("the extraction quota must move the derivation policy digest, or a workspace would keep facts derived without it")
	}
	encoded, err := json.Marshal(withQuota(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "quota") {
		t.Fatalf("a workspace without the quota marshals a key for it: %s", encoded)
	}
}
