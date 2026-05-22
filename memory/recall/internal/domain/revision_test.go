package domain

import (
	"testing"
	"time"
)

// TestAttachRevision pins the metadata-roundtrip contract: writing
// then reading back must round-trip Kind + SourceFactID for each
// revision kind, and unrelated metadata keys must be preserved.
func TestAttachRevision_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		rev  Revision
	}{
		{"supersede_no_source", Revision{Kind: RevisionSupersede}},
		{"fork_with_source", Revision{Kind: RevisionFork, SourceFactID: "src-1"}},
		{"contest_with_source", Revision{Kind: RevisionContest, SourceFactID: "src-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := TemporalFact{ID: "f1", Metadata: map[string]any{"unrelated": "keep"}}
			AttachRevision(&f, tc.rev)
			if f.Metadata["unrelated"] != "keep" {
				t.Errorf("unrelated metadata clobbered: %+v", f.Metadata)
			}
			got, ok := RevisionOf(f)
			if !ok {
				t.Fatal("RevisionOf reported absent after AttachRevision")
			}
			if got.Kind != tc.rev.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.rev.Kind)
			}
			if got.SourceFactID != tc.rev.SourceFactID {
				t.Errorf("SourceFactID = %q, want %q", got.SourceFactID, tc.rev.SourceFactID)
			}
		})
	}
}

func TestAttachRevision_NilFactOrEmptyKindIsNoOp(t *testing.T) {
	AttachRevision(nil, Revision{Kind: RevisionFork}) // must not panic

	f := TemporalFact{}
	AttachRevision(&f, Revision{Kind: ""})
	if f.Metadata != nil {
		t.Errorf("empty Kind must not allocate metadata, got %+v", f.Metadata)
	}
}

func TestRevisionOf_NoMetadataOrMissing(t *testing.T) {
	if _, ok := RevisionOf(TemporalFact{}); ok {
		t.Error("nil metadata → no revision")
	}
	if _, ok := RevisionOf(TemporalFact{Metadata: map[string]any{"k": "v"}}); ok {
		t.Error("unrelated metadata → no revision")
	}
}

func TestVersionFromFact(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	f := TemporalFact{
		ID:          "f1",
		ObservedAt:  t1,
		ValidFrom:   &t2,
		ValidTo:     &t2,
		CorrectedBy: "f2",
		Supersedes:  []string{"f0"},
	}
	AttachRevision(&f, Revision{Kind: RevisionFork})
	v := VersionFromFact(f)
	if !v.ValidFrom.Equal(t2) {
		t.Errorf("ValidFrom prefers fact ValidFrom, got %v", v.ValidFrom)
	}
	if !v.ValidTo.Equal(t2) {
		t.Errorf("ValidTo passthrough, got %v", v.ValidTo)
	}
	if v.SupersededBy != "f2" {
		t.Errorf("SupersededBy = %q", v.SupersededBy)
	}
	if v.Reason != string(RevisionFork) {
		t.Errorf("Reason = %q, want %q", v.Reason, RevisionFork)
	}
	// Mutating returned Supersedes must not bleed into the source.
	v.Supersedes[0] = "MUT"
	if f.Supersedes[0] != "f0" {
		t.Errorf("VersionFromFact must clone Supersedes, source mutated to %v", f.Supersedes)
	}
}

func TestVersionFromFact_FallsBackToObservedAt(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := VersionFromFact(TemporalFact{ID: "f1", ObservedAt: t1})
	if !v.ValidFrom.Equal(t1) {
		t.Errorf("missing ValidFrom must fall back to ObservedAt, got %v", v.ValidFrom)
	}
	if !v.ValidTo.IsZero() {
		t.Errorf("missing ValidTo must stay zero, got %v", v.ValidTo)
	}
}
