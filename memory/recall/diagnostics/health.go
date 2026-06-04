package diagnostics

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// PipelineHealth aggregates per-stage health over many Save and Recall calls.
type PipelineHealth struct {
	SaveSamples               int
	RecallSamples             int
	InputFacts                int
	InputCoverage             InputCoverage
	SavesWithObservedAt       int
	StructurizerCoverage      diagnostic.StructurizerCoverage
	ExtractorGuard            diagnostic.ExtractorGuard
	ProposalLifecycle         diagnostic.ProposalLifecycleDetail
	RecentMessagesProvided    int
	ExistingFactHintsProvided int
	CompiledFacts             FactQuality
	AppendedFacts             FactQuality
	SaveDrops                 map[FailureStage]int
	SaveLatency               time.Duration
	SaveStageLatency          map[string]LatencyStats
	HitRenderability          HitRenderability
	RecallDrops               map[FailureStage]int
	RecallLatency             time.Duration
	RecallStageLatency        map[string]LatencyStats
	RecallSourceLatency       map[string]LatencyStats
	SourceActivation          map[string]int
	SourceReturned            map[string]int
	WinnersBySource           map[string]int
	SoleSourceWinners         map[string]int
	MultiSourceWinners        int
	NoProvenanceHits          int
}

// NewPipelineHealth returns an empty aggregator with maps initialized.
func NewPipelineHealth() *PipelineHealth {
	return &PipelineHealth{
		CompiledFacts:       FactQuality{ByKind: map[string]int{}, ByPolicyDecision: map[string]int{}},
		AppendedFacts:       FactQuality{ByKind: map[string]int{}, ByPolicyDecision: map[string]int{}},
		SaveDrops:           map[FailureStage]int{},
		SaveStageLatency:    map[string]LatencyStats{},
		RecallDrops:         map[FailureStage]int{},
		RecallStageLatency:  map[string]LatencyStats{},
		RecallSourceLatency: map[string]LatencyStats{},
		SourceActivation:    map[string]int{},
		SourceReturned:      map[string]int{},
		WinnersBySource:     map[string]int{},
		SoleSourceWinners:   map[string]int{},
	}
}

// RecordSave folds a Save diagnostic into the aggregate.
func (p *PipelineHealth) RecordSave(diag SaveDiagnostics) {
	p.SaveSamples++
	p.SaveLatency += diag.TotalLatency
	p.InputFacts += diag.Input
	mergeInputCoverage(&p.InputCoverage, diag.InputCoverage)
	if diag.InputCoverage.HasObservedAt {
		p.SavesWithObservedAt++
	}
	mergeStructurizerCoverage(&p.StructurizerCoverage, diag.StructurizerCoverage)
	mergeExtractorGuard(&p.ExtractorGuard, diag.ExtractorGuard)
	mergeProposalLifecycle(&p.ProposalLifecycle, diag.ProposalLifecycle)
	p.RecentMessagesProvided += diag.RecentMessagesProvided
	p.ExistingFactHintsProvided += diag.ExistingFactHintsProvided
	mergeFactQuality(&p.CompiledFacts, diag.Compiled)
	mergeFactQuality(&p.AppendedFacts, diag.Appended)
	for stage, n := range diag.DropsByStage {
		p.SaveDrops[stage] += n
	}
	for stage, d := range diag.StageLatency {
		mergeLatencySample(p.SaveStageLatency, stage, d)
	}
}

// RecordRecall folds a Recall diagnostic into the aggregate.
func (p *PipelineHealth) RecordRecall(diag RecallDiagnostics) {
	p.RecallSamples++
	p.RecallLatency += diag.TotalLatency
	for stage, d := range diag.StageLatency {
		mergeLatencySample(p.RecallStageLatency, stage, d)
	}
	for source, d := range diag.SourceLatency {
		mergeLatencySample(p.RecallSourceLatency, source, d)
	}
	p.HitRenderability.Total += diag.HitRenderability.Total
	p.HitRenderability.EmptyRenderable += diag.HitRenderability.EmptyRenderable
	p.HitRenderability.StructuredOnly += diag.HitRenderability.StructuredOnly
	p.HitRenderability.GroundedEvidence += diag.HitRenderability.GroundedEvidence
	p.HitRenderability.EmptyTop += diag.HitRenderability.EmptyTop
	for stage, n := range diag.DropsByStage {
		p.RecallDrops[stage] += n
	}
	for _, src := range diag.Sources {
		if src.Activated {
			p.SourceActivation[src.Source]++
			p.SourceReturned[src.Source] += src.Returned
		}
	}
	for src, n := range diag.HitProvenance.WinnersBySource {
		p.WinnersBySource[src] += n
	}
	for src, n := range diag.HitProvenance.SoleSourceWinners {
		p.SoleSourceWinners[src] += n
	}
	p.MultiSourceWinners += diag.HitProvenance.MultiSourceWinners
	p.NoProvenanceHits += diag.HitProvenance.NoProvenance
}

func mergeProposalLifecycle(dst *diagnostic.ProposalLifecycleDetail, src diagnostic.ProposalLifecycleDetail) {
	if len(src.ByFamily) > 0 {
		if dst.ByFamily == nil {
			dst.ByFamily = map[string]diagnostic.ProposalFamilyLifecycle{}
		}
		for family, row := range src.ByFamily {
			existing := dst.ByFamily[family]
			existing.Proposed += row.Proposed
			existing.Grounded += row.Grounded
			existing.Promoted += row.Promoted
			existing.Rejected += row.Rejected
			existing.Overflow += row.Overflow
			existing.Omissions += row.Omissions
			dst.ByFamily[family] = existing
		}
	}
	dst.Grounding.Input += src.Grounding.Input
	dst.Grounding.Accepted += src.Grounding.Accepted
	dst.Grounding.Rejected += src.Grounding.Rejected
	mergeStringIntMap(&dst.Grounding.ByLevel, src.Grounding.ByLevel)
	mergeStringIntMap(&dst.Grounding.RejectReasons, src.Grounding.RejectReasons)
	dst.Arbitration.Input += src.Arbitration.Input
	dst.Arbitration.Winners += src.Arbitration.Winners
	dst.Arbitration.Losers += src.Arbitration.Losers
	mergeStringIntMap(&dst.Arbitration.RejectReasons, src.Arbitration.RejectReasons)
	dst.Promotion.Input += src.Promotion.Input
	dst.Promotion.Accepted += src.Promotion.Accepted
	dst.Promotion.Rejected += src.Promotion.Rejected
	mergeStringIntMap(&dst.Promotion.RejectReasons, src.Promotion.RejectReasons)
	dst.Compile.Input += src.Compile.Input
	dst.Compile.Compiled += src.Compile.Compiled
	dst.Compile.Rejected += src.Compile.Rejected
	mergeStringIntMap(&dst.Compile.RejectReasons, src.Compile.RejectReasons)
	dst.GraphDependency.Checked += src.GraphDependency.Checked
	dst.GraphDependency.Failed += src.GraphDependency.Failed
	mergeStringIntMap(&dst.GraphDependency.RejectReasons, src.GraphDependency.RejectReasons)
}

func mergeStringIntMap(dst *map[string]int, src map[string]int) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]int{}
	}
	for key, count := range src {
		(*dst)[key] += count
	}
}

func mergeExtractorGuard(dst *diagnostic.ExtractorGuard, src diagnostic.ExtractorGuard) {
	dst.Candidates += src.Candidates
	dst.Accepted += src.Accepted
	dst.Rejected += src.Rejected
	if len(src.ByReason) > 0 {
		if dst.ByReason == nil {
			dst.ByReason = map[string]int{}
		}
		for reason, count := range src.ByReason {
			dst.ByReason[reason] += count
		}
	}
	if len(src.ByFamily) > 0 {
		if dst.ByFamily == nil {
			dst.ByFamily = map[string]diagnostic.FamilyGuard{}
		}
		for family, row := range src.ByFamily {
			existing := dst.ByFamily[family]
			existing.Candidates += row.Candidates
			existing.Accepted += row.Accepted
			existing.Rejected += row.Rejected
			dst.ByFamily[family] = existing
		}
	}
	if len(src.RejectedProposals) > 0 {
		dst.RejectedProposals = append(dst.RejectedProposals, src.RejectedProposals...)
	}
}

func mergeStructurizerCoverage(dst *diagnostic.StructurizerCoverage, src diagnostic.StructurizerCoverage) {
	dst.TotalFactsSeen += src.TotalFactsSeen
	dst.KindFilled += src.KindFilled
	dst.EntitiesFilled += src.EntitiesFilled
	dst.SubjectFilled += src.SubjectFilled
	dst.ValidFromHintFilled += src.ValidFromHintFilled
}

func mergeInputCoverage(dst *InputCoverage, src InputCoverage) {
	dst.Facts += src.Facts
	dst.Turns += src.Turns
	dst.TurnsWithTypedTime += src.TurnsWithTypedTime
	dst.TurnsWithSpeaker += src.TurnsWithSpeaker
	dst.TurnsWithEvidenceID += src.TurnsWithEvidenceID
	dst.TurnsWithSessionID += src.TurnsWithSessionID
	dst.KnownEntities += src.KnownEntities
}

func mergeFactQuality(dst *FactQuality, src FactQuality) {
	dst.Total += src.Total
	dst.WithContent += src.WithContent
	dst.StructuredOnly += src.StructuredOnly
	dst.WithEvidence += src.WithEvidence
	dst.WithValidFrom += src.WithValidFrom
	dst.WithConfidence += src.WithConfidence
	dst.EmptyRenderable += src.EmptyRenderable
	if dst.ByKind == nil {
		dst.ByKind = map[string]int{}
	}
	for k, v := range src.ByKind {
		dst.ByKind[k] += v
	}
	if dst.ByPolicyDecision == nil {
		dst.ByPolicyDecision = map[string]int{}
	}
	for k, v := range src.ByPolicyDecision {
		dst.ByPolicyDecision[k] += v
	}
}
