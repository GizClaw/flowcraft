package diagnostics

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// PipelineHealth aggregates per-stage health over many Save and Recall calls.
type PipelineHealth struct {
	SaveSamples          int
	RecallSamples        int
	InputFacts           int
	InputCoverage        InputCoverage
	SavesWithObservedAt  int
	StructurizerCoverage diagnostic.StructurizerCoverage
	CompiledFacts        FactQuality
	AppendedFacts        FactQuality
	SaveDrops            map[FailureStage]int
	HitRenderability     HitRenderability
	RecallDrops          map[FailureStage]int
	RecallLatency        time.Duration
	SourceActivation     map[string]int
	SourceReturned       map[string]int
	WinnersBySource      map[string]int
	SoleSourceWinners    map[string]int
	MultiSourceWinners   int
	NoProvenanceHits     int
}

// NewPipelineHealth returns an empty aggregator with maps initialized.
func NewPipelineHealth() *PipelineHealth {
	return &PipelineHealth{
		CompiledFacts:     FactQuality{ByKind: map[string]int{}, ByPolicyDecision: map[string]int{}},
		AppendedFacts:     FactQuality{ByKind: map[string]int{}, ByPolicyDecision: map[string]int{}},
		SaveDrops:         map[FailureStage]int{},
		RecallDrops:       map[FailureStage]int{},
		SourceActivation:  map[string]int{},
		SourceReturned:    map[string]int{},
		WinnersBySource:   map[string]int{},
		SoleSourceWinners: map[string]int{},
	}
}

// RecordSave folds a Save diagnostic into the aggregate.
func (p *PipelineHealth) RecordSave(diag SaveDiagnostics) {
	p.SaveSamples++
	p.InputFacts += diag.Input
	mergeInputCoverage(&p.InputCoverage, diag.InputCoverage)
	if diag.InputCoverage.HasObservedAt {
		p.SavesWithObservedAt++
	}
	mergeStructurizerCoverage(&p.StructurizerCoverage, diag.StructurizerCoverage)
	mergeFactQuality(&p.CompiledFacts, diag.Compiled)
	mergeFactQuality(&p.AppendedFacts, diag.Appended)
	for stage, n := range diag.DropsByStage {
		p.SaveDrops[stage] += n
	}
}

// RecordRecall folds a Recall diagnostic into the aggregate.
func (p *PipelineHealth) RecordRecall(diag RecallDiagnostics) {
	p.RecallSamples++
	p.RecallLatency += diag.TotalLatency
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
