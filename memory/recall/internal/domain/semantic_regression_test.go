package domain

import "testing"

func TestSemanticFactClonePreservesStructuredFields(t *testing.T) {
	f := TemporalFact{
		ID:        "f1",
		Subject:   "Mira",
		Predicate: "visited",
		Object:    "Paris",
	}

	got := f.Clone()

	if got.Subject != "Mira" || got.Predicate != "visited" || got.Object != "Paris" {
		t.Fatalf("clone should preserve structured assertion fields, got %+v", got)
	}
}

func TestReasonEvidenceReturnsNeutralSummary(t *testing.T) {
	if got := ReasonEvidence([]QueryTaskIntent{QueryTaskYesNoVerification}, nil); got.Outcome != "unknown" {
		t.Fatalf("empty yes/no evidence should be unknown, got %q", got.Outcome)
	}

	evidence := ReasonEvidence([]QueryTaskIntent{QueryTaskYesNoVerification}, []EvidenceRow{{
		Subject: "Mira",
		Object:  "Paris",
	}})
	if evidence.Outcome != "evidence" || evidence.Evidence != 1 {
		t.Fatalf("yes/no evidence should stay neutral, got %+v", evidence)
	}
}
