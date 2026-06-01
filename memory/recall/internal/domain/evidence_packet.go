package domain

// EvidencePacket is the answer-facing graph packet assembled from canonical
// ledger objects. It keeps assertions, raw observations, and links distinct so
// answer layers can tell direct evidence from linked support instead of reading
// an opaque list of TemporalFacts.
type EvidencePacket struct {
	Primary      CandidateRef
	Assertions   []TemporalFact
	Observations []Observation
	Links        []FactLink
	EvidenceRefs []EvidenceRef
}

func (p EvidencePacket) Clone() EvidencePacket {
	out := EvidencePacket{
		Primary:      p.Primary,
		Assertions:   make([]TemporalFact, 0, len(p.Assertions)),
		Observations: make([]Observation, 0, len(p.Observations)),
		Links:        make([]FactLink, 0, len(p.Links)),
		EvidenceRefs: cloneEvidence(p.EvidenceRefs),
	}
	for _, assertion := range p.Assertions {
		out.Assertions = append(out.Assertions, assertion.Clone())
	}
	for _, observation := range p.Observations {
		out.Observations = append(out.Observations, observation.Clone())
	}
	for _, link := range p.Links {
		out.Links = append(out.Links, link.Clone())
	}
	return out
}
