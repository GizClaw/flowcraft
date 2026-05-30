package domain

import "testing"

func TestSemanticFactClonePreservesAssertionFields(t *testing.T) {
	f := TemporalFact{
		ID:        "f1",
		Polarity:  PolarityNegated,
		Modality:  ModalityCanceled,
		Certainty: CertaintyExplicit,
	}

	got := f.Clone()

	if got.Polarity != PolarityNegated || got.Modality != ModalityCanceled || got.Certainty != CertaintyExplicit {
		t.Fatalf("clone should preserve assertion metadata, got %+v", got)
	}
}

func TestReasonEvidenceCalibratesYesNoUnknown(t *testing.T) {
	if got := ReasonEvidence([]QueryTaskIntent{QueryTaskYesNoVerification}, nil); got.Outcome != "unknown" {
		t.Fatalf("empty yes/no evidence should be unknown, got %q", got.Outcome)
	}

	no := ReasonEvidence([]QueryTaskIntent{QueryTaskYesNoVerification}, []EvidenceRow{{
		Polarity: PolarityNegated,
		Modality: ModalityActual,
	}})
	if no.Outcome != "no" || no.Negated != 1 {
		t.Fatalf("negated evidence should determine no, got %+v", no)
	}
}
