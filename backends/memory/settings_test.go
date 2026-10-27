package memory

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/profile"
)

// TestProfileSettingsTravelWithTheBlock pins the three things the block's
// configuration decides: the settings resolve to the module's defaults, the
// bound is checked against the module's ceiling, and the footprint names the
// block only while it is served -- a deployment that leaves it off has to keep
// the footprint it had, because it serves what it served before.
func TestProfileSettingsTravelWithTheBlock(t *testing.T) {
	settings := Settings{}
	settings.applyDefaults()
	if settings.Profile.MaxTokens != profile.DefaultMaxTokens ||
		settings.Profile.MaxEntries != profile.DefaultMaxEntries {
		t.Fatalf("profile defaults = %+v", settings.Profile)
	}
	off, err := settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "profile") {
		t.Fatalf("a disabled block is in the footprint: %s", off)
	}

	settings.Profile.Enabled = true
	settings.Profile.MaxTokens, settings.Profile.MaxEntries = 256, 8
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	on, err := settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, `"profile":{"enabled":true,"max_tokens":256,"max_entries":8}`) {
		t.Fatalf("a served block is not in the footprint: %s", on)
	}
	if on == off {
		t.Fatal("serving the block did not change the footprint")
	}

	settings.Profile.MaxTokens = maxProfileTokens + 1
	if err := settings.Validate(); err == nil || !strings.Contains(err.Error(), "profile.max_tokens") {
		t.Fatalf("over-limit max_tokens accepted: %v", err)
	}
	settings.Profile.MaxTokens, settings.Profile.MaxEntries = 256, maxProfileEntries+1
	if err := settings.Validate(); err == nil || !strings.Contains(err.Error(), "profile.max_entries") {
		t.Fatalf("over-limit max_entries accepted: %v", err)
	}
}

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
	settings.Lanes.Cache = &CacheSettings{MaxBytes: 1 << 30}
	summary, err = settings.retrievalSummary()
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lanes":{"vector":{"weight":2},"bm25":{"weight":0.6},"entity":{"weight":0.4},"summary":{"weight":0.5},"fusion":"weighted","cache":{"max_bytes":1073741824}},"bm25":{"k1":1.4,"b":0.4},"retrieval":{"source_quotes":3,"rerank":true}}`
	if summary != want {
		t.Fatalf("tuned summary = %s, want %s", summary, want)
	}
}

// TestLaneCacheBudgetIsValidatedAndNamed pins the knob's shape: it is absent
// from the summary unless a host names it (so no existing run's fingerprint
// moves), and a budget in bytes cannot be negative.
func TestLaneCacheBudgetIsValidatedAndNamed(t *testing.T) {
	settings := Settings{}
	settings.applyDefaults()
	if settings.Lanes.Cache != nil {
		t.Fatal("an untouched workspace names a lane cache budget")
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("an untouched workspace does not validate: %v", err)
	}
	invalid := Settings{}
	invalid.applyDefaults()
	invalid.Lanes.Cache = &CacheSettings{MaxBytes: -1}
	if err := invalid.Validate(); err == nil {
		t.Fatal("a negative lane cache budget was accepted")
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

// TestGenerateTemperatureIsAPolicy pins the sampling pin. It has to move the
// derivation policy digest: temperature decides what the extractor writes, so a
// workspace derived without it cannot be reused by a deployment that pins it.
// An untouched workspace keeps its digest, because the absent key is the
// provider's default and therefore the policy every earlier generation was
// derived under.
func TestGenerateTemperatureIsAPolicy(t *testing.T) {
	withTemperature := func(temperature *float64) Settings {
		settings := Settings{}
		settings.applyDefaults()
		settings.Generate.Provider, settings.Generate.Name = "deepseek", "deepseek-flash"
		settings.Generate.Temperature = temperature
		return settings
	}
	defaultDigest, err := policyDigest(withTemperature(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	pinned, unpinned := 0.0, 1.0
	pinnedDigest, err := policyDigest(withTemperature(&pinned), "")
	if err != nil {
		t.Fatal(err)
	}
	if pinnedDigest == defaultDigest {
		t.Fatal("pinning the temperature must move the derivation policy digest, or a workspace derived at the provider default would be reused")
	}
	unpinnedDigest, err := policyDigest(withTemperature(&unpinned), "")
	if err != nil {
		t.Fatal(err)
	}
	if unpinnedDigest == pinnedDigest {
		t.Fatal("0 and 1 are different samplings and must not share a policy")
	}
	untouched := withTemperature(nil)
	untouched.Generate.Temperature = nil
	if err := untouched.Validate(); err != nil {
		t.Fatalf("a workspace that never named the key must still validate: %v", err)
	}
}

// TestGenerateTemperatureIsValidated rejects what the module cannot mean: a
// value outside the sampling range, a temperature with no model to spend it on
// (an embed block samples nothing), and a non-finite value.
func TestGenerateTemperatureIsValidated(t *testing.T) {
	build := func(temperature float64) Settings {
		settings := Settings{}
		settings.applyDefaults()
		settings.Generate.Provider, settings.Generate.Name = "deepseek", "deepseek-flash"
		settings.Generate.Temperature = &temperature
		return settings
	}
	for _, value := range []float64{-0.1, 2.1, math.Inf(1), math.NaN()} {
		if err := build(value).Validate(); err == nil {
			t.Fatalf("generate.temperature %v was accepted", value)
		}
	}
	for _, value := range []float64{0, 0.7, 2} {
		if err := build(value).Validate(); err != nil {
			t.Fatalf("generate.temperature %v was rejected: %v", value, err)
		}
	}
	orphan := build(0)
	orphan.Generate.Provider, orphan.Generate.Name = "", ""
	if err := orphan.Validate(); err == nil {
		t.Fatal("a temperature with no generate model was accepted")
	}
	embedded := Settings{}
	embedded.applyDefaults()
	cold := 0.0
	embedded.Embed.Provider, embedded.Embed.Name, embedded.Embed.Temperature = "openai", "text-embedding-3-small", &cold
	if err := embedded.Validate(); err == nil {
		t.Fatal("embed.temperature was accepted: an embedding is not sampled")
	}
}
