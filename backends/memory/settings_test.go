package memory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// TestTriggerSettingsAreValidated pins the fast path's settings: the poll
// period defaults to a second, "0" turns the trigger off without turning the
// sweep off, and a lease ttl or a batch bound that cannot mean anything is
// refused where the document is read rather than at the first poll.
func TestTriggerSettingsAreValidated(t *testing.T) {
	fresh := func() Settings {
		settings := Settings{}
		settings.applyDefaults()
		return settings
	}
	defaults := fresh()
	if defaults.Derive.Trigger.Interval != defaultTriggerInterval {
		t.Fatalf("the trigger's default period = %q, want %q",
			defaults.Derive.Trigger.Interval, defaultTriggerInterval)
	}
	period, err := defaults.triggerInterval()
	if err != nil {
		t.Fatal(err)
	}
	if period != time.Second {
		t.Fatalf("the trigger's default period = %v, want a second", period)
	}
	off := fresh()
	off.Derive.Trigger.Interval = "0"
	period, err = off.triggerInterval()
	if err != nil || period != 0 {
		t.Fatalf(`derive.trigger.interval "0" = %v, %v`, period, err)
	}
	// The ttl is the worker's default until it is named, and it has to bound
	// something: a claim that expires before it is written is not a claim.
	if ttl, err := fresh().triggerLeaseTTL(); err != nil || ttl != 0 {
		t.Fatalf("an unnamed lease ttl = %v, %v", ttl, err)
	}
	for _, value := range []string{"0s", "-1s"} {
		invalid := fresh()
		invalid.Derive.Trigger.LeaseTTL = value
		if _, err := invalid.triggerLeaseTTL(); err == nil {
			t.Fatalf("lease_ttl %q was accepted", value)
		}
	}
	invalid := fresh()
	invalid.Derive.Trigger.Interval = "whenever"
	if _, err := invalid.triggerInterval(); err == nil {
		t.Fatal("a period that is not a duration was accepted")
	} else if !strings.Contains(err.Error(), "derive.trigger.interval") {
		t.Fatalf("the refusal does not name the setting: %v", err)
	}
	negative := fresh()
	negative.Derive.Trigger.Batch = -1
	if err := negative.Validate(); err == nil {
		t.Fatal("a negative batch bound was accepted")
	}
}

// TestTriggerIntervalReachesTheRunner wires the switch through the factory: the
// trigger is built whether or not its runner polls -- the write paths record
// the same work either way -- and the interval is what the assembly reads when
// it decides to run the polls. The polls ride on the sweep's runner, so a
// deployment that turns the sweep off ("interval": "0") runs neither, which is
// what the absent run context says.
func TestTriggerIntervalReachesTheRunner(t *testing.T) {
	manual, _ := newTestAssemblyWith(t, triggerSettings)
	t.Cleanup(func() { _ = manual.Close() })
	if manual.triggerInterval != 0 {
		t.Fatalf("triggerInterval = %v, want the runner off", manual.triggerInterval)
	}
	if manual.trigger == nil || manual.journal == nil {
		t.Fatal("turning the runner off must leave the journal and the consumer built")
	}
	if manual.cancel != nil {
		t.Fatal(`"interval": "0" started a runner`)
	}
	polled, _ := newTestAssemblyWith(t, `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "interval": "1m"
}`)
	t.Cleanup(func() { _ = polled.Close() })
	if polled.triggerInterval != time.Second {
		t.Fatalf("triggerInterval = %v, want the default second", polled.triggerInterval)
	}
	if polled.cancel == nil {
		t.Fatal("a sweep with the trigger left on started no runner")
	}
}
