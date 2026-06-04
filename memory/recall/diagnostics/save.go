package diagnostics

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// FactQuality summarises compiled fact shape for one Save call.
type FactQuality struct {
	Total            int
	WithContent      int
	StructuredOnly   int
	WithEvidence     int
	WithValidFrom    int
	WithConfidence   int
	EmptyRenderable  int
	ByKind           map[string]int
	ByPolicyDecision map[string]int
}

// InputCoverage quantifies SaveRequest channel coverage.
type InputCoverage struct {
	Facts               int
	Turns               int
	TurnsWithTypedTime  int
	TurnsWithSpeaker    int
	TurnsWithEvidenceID int
	TurnsWithSessionID  int
	KnownEntities       int
	HasObservedAt       bool
}

// SaveDiagnostics is the per-stage health view of one Save call.
type SaveDiagnostics struct {
	Input                     int
	InputCoverage             InputCoverage
	StructurizerCoverage      diagnostic.StructurizerCoverage
	ExtractorTokenUsage       ExtractorTokenUsage
	ExtractorGuard            ExtractorGuard
	ProposalLifecycle         diagnostic.ProposalLifecycleDetail
	RecentMessagesProvided    int
	ExistingFactHintsProvided int
	Compiled                  FactQuality
	Appended                  FactQuality
	DropsByStage              map[FailureStage]int
	TotalLatency              time.Duration
	StageLatency              map[string]time.Duration
	Attributions              []Attribution
}

type TokenUsage = diagnostic.TokenUsage
type ExtractorStageTokenUsage = diagnostic.ExtractorStageTokenUsage
type ExtractorTokenUsage = diagnostic.ExtractorTokenUsage
type ExtractorGuard = diagnostic.ExtractorGuard
type GuardedSemanticProposal = diagnostic.GuardedSemanticProposal
type ProposalLifecycleDetail = diagnostic.ProposalLifecycleDetail

// DiagnoseSave produces a per-stage health view from trace.Stages.
// The request is inspected for typed-channel coverage (Tier / Turns
// / EvidenceID); everything else is reconstructed from stages.
func DiagnoseSave(req domain.SaveRequest, trace domain.SaveTrace) SaveDiagnostics {
	stages := trace.Stages
	cov := inputCoverage(req, stages)
	out := SaveDiagnostics{
		Input:                     cov.Facts + cov.Turns,
		InputCoverage:             cov,
		StructurizerCoverage:      diagnostic.ExtractStructurizerCoverage(stages),
		ExtractorTokenUsage:       extractorTokenUsage(stages),
		ExtractorGuard:            extractorGuard(stages),
		ProposalLifecycle:         proposalLifecycle(stages),
		RecentMessagesProvided:    ingestContextCoverage(stages).RecentMessages,
		ExistingFactHintsProvided: ingestContextCoverage(stages).ExistingFactHints,
		Compiled:                  factQualityFromIngest(stages),
		Appended:                  factQualityFromResolve(stages),
		TotalLatency:              SaveLatency(trace),
		StageLatency:              stageLatencies(stages),
		Attributions:              AttributeSaveTrace(trace),
	}
	if len(out.Attributions) > 0 {
		out.DropsByStage = make(map[FailureStage]int, len(out.Attributions))
		for _, a := range out.Attributions {
			out.DropsByStage[a.Stage]++
		}
	}
	return out
}

func inputCoverage(req domain.SaveRequest, stages []diagnostic.StageDiagnostic) InputCoverage {
	cov := InputCoverage{
		Facts:         len(req.Facts),
		KnownEntities: diagnostic.ExtractKnownEntitiesSeen(stages),
		HasObservedAt: !req.ObservedAt.IsZero(),
	}
	for _, t := range req.Turns {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		cov.Turns++
		if !t.Time.IsZero() {
			cov.TurnsWithTypedTime++
		}
		if strings.TrimSpace(t.Speaker) != "" {
			cov.TurnsWithSpeaker++
		}
		if strings.TrimSpace(t.EvidenceID) != "" {
			cov.TurnsWithEvidenceID++
		}
		if strings.TrimSpace(t.SessionID) != "" {
			cov.TurnsWithSessionID++
		}
	}
	return cov
}

func proposalLifecycle(stages []diagnostic.StageDiagnostic) diagnostic.ProposalLifecycleDetail {
	var out diagnostic.ProposalLifecycleDetail
	for _, st := range stages {
		if st.Stage != "ingest" && st.Stage != "structured_ingest" {
			continue
		}
		d, ok := st.Detail.(diagnostic.IngestDetail)
		if !ok {
			continue
		}
		mergeProposalLifecycle(&out, d.ProposalLifecycle)
	}
	for _, st := range stages {
		if st.Stage != "graph_dependencies" {
			continue
		}
		d, ok := st.Detail.(diagnostic.GraphDependencyDetail)
		if !ok {
			continue
		}
		out.GraphDependency.Checked += d.Checked
		if d.FailedReason != "" {
			out.GraphDependency.Failed++
			if out.GraphDependency.RejectReasons == nil {
				out.GraphDependency.RejectReasons = map[string]int{}
			}
			out.GraphDependency.RejectReasons[d.FailedReason]++
		}
	}
	return out
}

type ingestContextCounts struct {
	RecentMessages    int
	ExistingFactHints int
}

func ingestContextCoverage(stages []diagnostic.StageDiagnostic) ingestContextCounts {
	var out ingestContextCounts
	for _, st := range stages {
		if st.Stage != "ingest" && st.Stage != "structured_ingest" {
			continue
		}
		d, ok := st.Detail.(diagnostic.IngestDetail)
		if !ok {
			continue
		}
		if d.RecentMessagesProvided > out.RecentMessages {
			out.RecentMessages = d.RecentMessagesProvided
		}
		if d.ExistingFactHintsProvided > out.ExistingFactHints {
			out.ExistingFactHints = d.ExistingFactHintsProvided
		}
	}
	return out
}

func extractorGuard(stages []diagnostic.StageDiagnostic) ExtractorGuard {
	var out ExtractorGuard
	for _, st := range stages {
		if st.Stage != "ingest" && st.Stage != "structured_ingest" {
			continue
		}
		d, ok := st.Detail.(diagnostic.IngestDetail)
		if !ok {
			continue
		}
		mergeExtractorGuard(&out, d.ExtractorGuard)
	}
	return out
}

func extractorTokenUsage(stages []diagnostic.StageDiagnostic) ExtractorTokenUsage {
	for _, st := range stages {
		if st.Stage != "ingest" && st.Stage != "structured_ingest" {
			continue
		}
		d, ok := st.Detail.(diagnostic.IngestDetail)
		if !ok {
			continue
		}
		return d.ExtractorTokenUsage
	}
	return ExtractorTokenUsage{}
}

// factQualityFromIngest reads the precomputed FactStats off the
// ingest stage's Detail. Stage emission now lives in the pipeline
// framework, which avoids the per-fact walk this function used to do;
// FactStats is now computed inside the Ingest stage
// (which has the domain import diagnostic/ cannot take) and embedded
// in IngestDetail so per-Save quality survives the refactor.
//
// The "structured_ingest" stage (async semantic write) emits the
// same IngestDetail shape, so we accept either name and prefer the
// one that actually carried facts (Total > 0).
func factQualityFromIngest(stages []diagnostic.StageDiagnostic) FactQuality {
	var out FactQuality
	for _, st := range stages {
		if st.Stage != "ingest" && st.Stage != "structured_ingest" {
			continue
		}
		d, ok := st.Detail.(diagnostic.IngestDetail)
		if !ok {
			continue
		}
		q := factQualityFromStats(d.FactStats)
		if q.Total == 0 {
			q.Total = d.ExtractedFacts
		}
		mergeFactQuality(&out, q)
	}
	return out
}

func factQualityFromResolve(stages []diagnostic.StageDiagnostic) FactQuality {
	for _, st := range stages {
		if st.Stage != "resolve" {
			continue
		}
		d, ok := st.Detail.(diagnostic.ResolveDetail)
		if !ok {
			continue
		}
		q := factQualityFromStats(d.FactStats)
		if q.Total == 0 {
			q.Total = d.Appended
		}
		return q
	}
	return FactQuality{}
}

func factQualityFromStats(s diagnostic.FactStats) FactQuality {
	q := FactQuality{
		Total:           s.Total,
		WithContent:     s.WithContent,
		StructuredOnly:  s.StructuredOnly,
		EmptyRenderable: s.EmptyRenderable,
		WithEvidence:    s.WithEvidence,
		WithValidFrom:   s.WithValidFrom,
		WithConfidence:  s.WithConfidence,
	}
	if len(s.ByKind) > 0 {
		q.ByKind = make(map[string]int, len(s.ByKind))
		for k, v := range s.ByKind {
			q.ByKind[k] += v
		}
	}
	return q
}

// SaveLatency aggregates per-stage Duration for the write trace.
func SaveLatency(trace domain.SaveTrace) time.Duration {
	var d time.Duration
	for _, st := range trace.Stages {
		d += st.Duration
	}
	return d
}
