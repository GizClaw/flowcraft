package memory

import (
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
	summary, err = settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lanes":{"vector":{"weight":2},"bm25":{"weight":0.6},"entity":{"weight":0.4},"summary":{"weight":0.5},"fusion":"weighted"},"bm25":{"k1":1.4,"b":0.4},"retrieval":{"source_quotes":3,"rerank":true}}`
	if summary != want {
		t.Fatalf("tuned summary = %s, want %s", summary, want)
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
