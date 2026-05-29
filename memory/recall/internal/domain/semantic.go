package domain

import "strings"

// Polarity captures whether an assertion affirms or explicitly negates a
// proposition. "Unknown" is for explicitly unknown source assertions or
// query-time no-evidence reasoning; extractors must not write it merely because
// evidence is absent.
type Polarity string

const (
	PolarityAffirmed Polarity = "affirmed"
	PolarityNegated  Polarity = "negated"
	PolarityUnknown  Polarity = "unknown"
)

// NormalizePolarity returns the canonical polarity. Empty defaults to affirmed
// so older facts keep their historical positive-fact semantics.
func NormalizePolarity(p Polarity) Polarity {
	switch p {
	case PolarityNegated, PolarityUnknown:
		return p
	default:
		return PolarityAffirmed
	}
}

// Modality records whether the assertion describes actual reality, a plan,
// hypothesis, canceled event, preference/desire, or suggestion.
type Modality string

const (
	ModalityActual         Modality = "actual"
	ModalityPlanned        Modality = "planned"
	ModalityHypothetical   Modality = "hypothetical"
	ModalityCounterfactual Modality = "counterfactual"
	ModalityCanceled       Modality = "canceled"
	ModalityDesired        Modality = "desired"
	ModalitySuggested      Modality = "suggested"
)

func NormalizeModality(m Modality) Modality {
	switch m {
	case ModalityPlanned, ModalityHypothetical, ModalityCounterfactual, ModalityCanceled, ModalityDesired, ModalitySuggested:
		return m
	default:
		return ModalityActual
	}
}

// Certainty distinguishes directly stated assertions from weaker model
// inferences. Extractors should prefer explicit unless the source itself uses
// uncertain language.
type Certainty string

const (
	CertaintyExplicit  Certainty = "explicit"
	CertaintyInferred  Certainty = "inferred"
	CertaintyLikely    Certainty = "likely"
	CertaintyUncertain Certainty = "uncertain"
)

func NormalizeCertainty(c Certainty) Certainty {
	switch c {
	case CertaintyInferred, CertaintyLikely, CertaintyUncertain:
		return c
	default:
		return CertaintyExplicit
	}
}

// NormalizeSemantic fills defaults and canonicalises lightweight slots. It is
// idempotent and safe to call anywhere a fact crosses a stage boundary.
func NormalizeSemantic(f TemporalFact) TemporalFact {
	f.Polarity = NormalizePolarity(f.Polarity)
	f.Modality = NormalizeModality(f.Modality)
	f.Certainty = NormalizeCertainty(f.Certainty)
	return f
}

// SemanticTextBlob returns a compact text representation for retrieval/ranking
// features that need to include structured assertion slots.
func SemanticTextBlob(f TemporalFact) string {
	var parts []string
	if f.Polarity != "" && f.Polarity != PolarityAffirmed {
		parts = append(parts, string(NormalizePolarity(f.Polarity)))
	}
	if f.Modality != "" && f.Modality != ModalityActual {
		parts = append(parts, string(NormalizeModality(f.Modality)))
	}
	if f.Certainty != "" && f.Certainty != CertaintyExplicit {
		parts = append(parts, string(NormalizeCertainty(f.Certainty)))
	}
	return strings.Join(parts, " ")
}
