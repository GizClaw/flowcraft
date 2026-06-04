package domain

import "time"

// DiscoverySignal records why a source or lane nominated a candidate. It is
// provenance for discovery diagnostics and bounded priors, not final relevance.
type DiscoverySignal struct {
	Source string
	Kind   string
	Value  string
	Score  float64
}

// CandidateEnvelope keeps discovery, assessment, rank, and pack decisions in
// separate slots so source-local score is not reused as semantic relevance.
type CandidateEnvelope struct {
	Candidate Candidate
	Item      ContextItem

	DiscoverySignals []DiscoverySignal
	DiscoveryScore   float64
	DiscoveryRank    int
	DiscoverySource  string

	Assessment   CandidateAssessment
	RankScore    float64
	PackDecision PackDecision
}

// CandidateAssessment is the read-side semantic usefulness decision for a
// materialized candidate.
type CandidateAssessment struct {
	RelevanceScore  float64
	SupportScore    float64
	LiteralScore    float64
	StructuredScore float64
	SemanticScore   float64
	SourcePrior     float64

	HardConstraintPass bool
	Confidence         float64
	Reason             string
	DropReason         string
	FallbackReason     string

	EquivalenceGroup string
	SupportGroup     string
	DiversityGroup   string
}

// AssessmentInput is the source-neutral input passed to the candidate assessor
// after materialization and policy filtering.
type AssessmentInput struct {
	QueryText string
	Intent    QueryIntent
	Item      ContextItem
	Evidence  []EvidenceRef
	Links     []FactLink
	Signals   []DiscoverySignal
	Now       time.Time
}

// PackDecision records how an assessed candidate was handled by context pack.
type PackDecision struct {
	Packed     bool
	Reason     string
	InputRank  int
	OutputRank int
}

func NewCandidateEnvelope(item ContextItem) CandidateEnvelope {
	candidate := item.Candidate
	if candidate.ID == "" && item.Ref.ID != "" {
		candidate = item.Ref
	}
	return CandidateEnvelope{
		Candidate:        candidate,
		Item:             item,
		DiscoverySignals: ContextItemDiscoverySignals(item),
		DiscoveryScore:   candidate.Score,
		DiscoveryRank:    candidate.Rank,
		DiscoverySource:  candidate.Source,
	}
}

func NewCandidateEnvelopes(items []ContextItem) []CandidateEnvelope {
	if len(items) == 0 {
		return nil
	}
	out := make([]CandidateEnvelope, 0, len(items))
	for _, item := range items {
		out = append(out, NewCandidateEnvelope(item))
	}
	return out
}

func CandidateDiscoverySignals(c Candidate) []DiscoverySignal {
	return append([]DiscoverySignal(nil), c.DiscoverySignals...)
}

func ContextItemDiscoverySignals(item ContextItem) []DiscoverySignal {
	if signals := CandidateDiscoverySignals(item.Candidate); len(signals) > 0 {
		return signals
	}
	return CandidateDiscoverySignals(item.Ref)
}

func SetCandidateDiscoverySignals(c *Candidate, signals []DiscoverySignal) {
	if c == nil {
		return
	}
	c.DiscoverySignals = append([]DiscoverySignal(nil), signals...)
}

func AddCandidateDiscoverySignal(c *Candidate, signal DiscoverySignal) {
	if c == nil || signal.Source == "" && signal.Kind == "" && signal.Value == "" && signal.Score == 0 {
		return
	}
	signals := CandidateDiscoverySignals(*c)
	signals = append(signals, signal)
	SetCandidateDiscoverySignals(c, signals)
}
