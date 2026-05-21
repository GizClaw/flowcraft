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
