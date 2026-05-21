package domain

import (
	"testing"
	"time"
)

// TestTemporalFact_Clone is the headline deep-copy contract: every
// reference-typed field on the original must be independent of the
// clone, so callers that mutate slices/maps after handing the fact
// off to canonical stores cannot retroactively corrupt the stored
// row.
func TestTemporalFact_Clone(t *testing.T) {
	valid := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := valid.Add(time.Hour)
	orig := TemporalFact{
		ID:               "f1",
		Entities:         []string{"alice"},
		Participants:     []string{"bob"},
		EvidenceRefs:     []EvidenceRef{{ID: "ev1"}},
		SourceMessageIDs: []string{"m1"},
		Supersedes:       []string{"s1"},
		MergeHints: MergeHints{
			SuggestedMergeKey: "k",
			Supersedes:        []string{"sm1"},
			Extra:             map[string]any{"a": 1},
		},
		Metadata:  map[string]any{"k": "v"},
		ValidFrom: &valid,
		ValidTo:   &valid,
		ExpiresAt: &expires,
	}

	cp := orig.Clone()

	cp.Entities[0] = "MUT"
	cp.Participants[0] = "MUT"
	cp.EvidenceRefs[0].ID = "MUT"
	cp.SourceMessageIDs[0] = "MUT"
	cp.Supersedes[0] = "MUT"
	cp.MergeHints.Supersedes[0] = "MUT"
	cp.MergeHints.Extra["a"] = 999
	cp.Metadata["k"] = "MUT"
	*cp.ValidFrom = valid.Add(time.Hour)
	*cp.ValidTo = valid.Add(time.Hour)
	*cp.ExpiresAt = expires.Add(time.Hour)

	if orig.Entities[0] != "alice" {
		t.Errorf("Entities aliased: %v", orig.Entities)
	}
	if orig.Participants[0] != "bob" {
		t.Errorf("Participants aliased: %v", orig.Participants)
	}
	if orig.EvidenceRefs[0].ID != "ev1" {
		t.Errorf("EvidenceRefs aliased: %v", orig.EvidenceRefs)
	}
	if orig.SourceMessageIDs[0] != "m1" {
		t.Errorf("SourceMessageIDs aliased: %v", orig.SourceMessageIDs)
	}
	if orig.Supersedes[0] != "s1" {
		t.Errorf("Supersedes aliased: %v", orig.Supersedes)
	}
	if orig.MergeHints.Supersedes[0] != "sm1" {
		t.Errorf("MergeHints.Supersedes aliased: %v", orig.MergeHints.Supersedes)
	}
	if orig.MergeHints.Extra["a"] != 1 {
		t.Errorf("MergeHints.Extra aliased: %v", orig.MergeHints.Extra)
	}
	if orig.Metadata["k"] != "v" {
		t.Errorf("Metadata aliased: %v", orig.Metadata)
	}
	if !orig.ValidFrom.Equal(valid) {
		t.Errorf("ValidFrom aliased: %v", orig.ValidFrom)
	}
	if !orig.ValidTo.Equal(valid) {
		t.Errorf("ValidTo aliased: %v", orig.ValidTo)
	}
	if !orig.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt aliased: %v", orig.ExpiresAt)
	}
}

// TestClone_PreservesOrigin pins the F.1a deep-copy contract for
// FactOrigin.EpisodeFactIDs — the clone must own an independent slice
// so worker-side mutation of the snapshot cannot retroactively edit
// the stored row.
func TestClone_PreservesOrigin(t *testing.T) {
	orig := TemporalFact{
		ID: "f1",
		Origin: FactOrigin{
			RequestID:      "req-1",
			Kind:           OriginKindSemanticDerivation,
			EpisodeFactIDs: []string{"e1", "e2"},
		},
	}
	cp := orig.Clone()
	cp.Origin.EpisodeFactIDs[0] = "MUT"
	if orig.Origin.EpisodeFactIDs[0] != "e1" {
		t.Errorf("Origin.EpisodeFactIDs aliased: %v", orig.Origin.EpisodeFactIDs)
	}
	if cp.Origin.RequestID != "req-1" || cp.Origin.Kind != OriginKindSemanticDerivation {
		t.Errorf("scalar Origin fields lost on clone: %+v", cp.Origin)
	}
}

func TestTemporalFact_Clone_NilFieldsStayNil(t *testing.T) {
	orig := TemporalFact{ID: "f1"}
	cp := orig.Clone()
	if cp.Entities != nil || cp.Participants != nil || cp.EvidenceRefs != nil {
		t.Errorf("nil slices must round-trip nil, got %+v", cp)
	}
	if cp.Metadata != nil {
		t.Errorf("nil metadata must round-trip nil, got %+v", cp.Metadata)
	}
	if cp.ValidFrom != nil || cp.ValidTo != nil || cp.ExpiresAt != nil {
		t.Errorf("nil time pointers must round-trip nil, got %+v", cp)
	}
}

func TestIsSuperseded(t *testing.T) {
	if IsSuperseded(TemporalFact{}) {
		t.Error("empty fact is not superseded")
	}
	if !IsSuperseded(TemporalFact{CorrectedBy: "f2"}) {
		t.Error("CorrectedBy != \"\" → superseded")
	}
}

func TestIsActive(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	if !IsActive(TemporalFact{}, now) {
		t.Error("open-ended fact is active")
	}
	if !IsActive(TemporalFact{ValidTo: &future}, now) {
		t.Error("future ValidTo → still active")
	}
	if IsActive(TemporalFact{ValidTo: &past}, now) {
		t.Error("past ValidTo → not active")
	}
	if IsActive(TemporalFact{CorrectedBy: "f2"}, now) {
		t.Error("superseded → not active even if open-ended")
	}
}

// TestIsProjectable_ActiveFactPasses pins the happy path: an open-ended,
// not-superseded, not-closed, not-expired fact must be projectable.
func TestIsProjectable_ActiveFactPasses(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !IsProjectable(TemporalFact{ID: "f1"}, now) {
		t.Error("plain open-ended fact must be projectable")
	}
}

// TestIsProjectable_RespectsClosed pins the soft-forget invariant:
// projections must NOT index facts whose Closed=true, even when they
// remain canonical-active. This is the bug Cluster B fixes — before
// the predicate split, profile/relation/graph would retain closed
// facts until the next RebuildAll.
func TestIsProjectable_RespectsClosed(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	f := TemporalFact{ID: "f1", Closed: true}
	if !IsCanonicalActive(f, now) {
		t.Fatal("guard: closed fact should still be canonical-active")
	}
	if IsProjectable(f, now) {
		t.Error("Closed=true → must not be projectable")
	}
}

// TestIsProjectable_RespectsExpiresAt pins the TTL invariant:
// projections must drop facts past their ExpiresAt instant.
func TestIsProjectable_RespectsExpiresAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	expired := TemporalFact{ID: "f1", ExpiresAt: &past}
	if !IsCanonicalActive(expired, now) {
		t.Fatal("guard: expired-only fact should still be canonical-active")
	}
	if IsProjectable(expired, now) {
		t.Error("ExpiresAt in past → must not be projectable")
	}
	live := TemporalFact{ID: "f2", ExpiresAt: &future}
	if !IsProjectable(live, now) {
		t.Error("ExpiresAt in future → still projectable")
	}
}

// TestIsProjectable_RespectsSuperseded pins the canonical layer: a
// fact with a successor (CorrectedBy != "") must not be projectable
// regardless of Closed/ExpiresAt state.
func TestIsProjectable_RespectsSuperseded(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if IsProjectable(TemporalFact{ID: "f1", CorrectedBy: "f2"}, now) {
		t.Error("superseded fact must not be projectable")
	}
}

// TestIsHistorical_KeepsPastValidTo pins the LoCoMo regression fix
// (2026-05-21): historical projections (timeline / retrieval / entity
// / graph) MUST keep events whose ValidTo has long since closed,
// otherwise "When did X happen?" queries lose the underlying event.
// IsProjectable conflates "currently-active state" with "indexable
// historical fact"; IsHistorical separates the two so the active-slot
// views (profile / relation) keep their semantics while the four
// historical views index the full observed record.
func TestIsHistorical_KeepsPastValidTo(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-365 * 24 * time.Hour)
	f := TemporalFact{ID: "ev1", ObservedAt: past, ValidTo: &past}
	if IsProjectable(f, now) {
		t.Fatal("guard: past-ValidTo fact must not be projectable (active-slot view)")
	}
	if !IsHistorical(f, now) {
		t.Error("past-ValidTo fact must remain historical for timeline/retrieval/entity/graph")
	}
}

func TestIsHistorical_DropsSupersededAndRetired(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if IsHistorical(TemporalFact{ID: "f1", CorrectedBy: "f2"}, now) {
		t.Error("superseded fact must not be historical")
	}
	if IsHistorical(TemporalFact{ID: "f1", Closed: true}, now) {
		t.Error("closed fact must not be historical")
	}
	past := now.Add(-time.Hour)
	if IsHistorical(TemporalFact{ID: "f1", ExpiresAt: &past}, now) {
		t.Error("TTL-expired fact must not be historical")
	}
}

func TestIsRetired(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	if IsRetired(TemporalFact{}, now) {
		t.Error("open fact not retired")
	}
	if !IsRetired(TemporalFact{Closed: true}, now) {
		t.Error("Closed → retired")
	}
	if !IsRetired(TemporalFact{ExpiresAt: &past}, now) {
		t.Error("past ExpiresAt → retired")
	}
	if IsRetired(TemporalFact{ExpiresAt: &future}, now) {
		t.Error("future ExpiresAt → not yet retired")
	}
}

func TestEffectiveTimestamp(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if got := EffectiveTimestamp(TemporalFact{ObservedAt: t1}); !got.Equal(t1) {
		t.Errorf("ObservedAt fallback: %v", got)
	}
	if got := EffectiveTimestamp(TemporalFact{ObservedAt: t1, ValidFrom: &t2}); !got.Equal(t2) {
		t.Errorf("ValidFrom wins over ObservedAt: %v", got)
	}
	zero := time.Time{}
	if got := EffectiveTimestamp(TemporalFact{ObservedAt: t1, ValidFrom: &zero}); !got.Equal(t1) {
		t.Errorf("zero ValidFrom falls through to ObservedAt: %v", got)
	}
}
