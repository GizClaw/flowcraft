package diagnostics

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
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
	Input                int
	InputCoverage        InputCoverage
	StructurizerCoverage diagnostic.StructurizerCoverage
	Compiled             FactQuality
	Appended             FactQuality
	DropsByStage         map[FailureStage]int
	Attributions         []Attribution
}

// DiagnoseSave produces a per-stage health view from trace.Stages.
// The request is inspected for typed-channel coverage (Tier / Turns
// / EvidenceID); everything else is reconstructed from stages.
func DiagnoseSave(req domain.SaveRequest, trace domain.SaveTrace) SaveDiagnostics {
	stages := trace.Stages
	cov := inputCoverage(req, stages)
	out := SaveDiagnostics{
		Input:                cov.Facts + cov.Turns,
		InputCoverage:        cov,
		StructurizerCoverage: diagnostic.ExtractStructurizerCoverage(stages),
		Compiled:             factQualityFromIngest(stages),
		Appended:             factQualityFromResolve(stages),
		Attributions:         AttributeSaveTrace(trace),
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

func factQualityFromIngest(stages []diagnostic.StageDiagnostic) FactQuality {
	for _, st := range stages {
		if st.Stage == "ingest" {
			if d, ok := st.Detail.(diagnostic.IngestDetail); ok {
				return FactQuality{Total: d.ExtractedFacts}
			}
		}
	}
	return FactQuality{}
}

func factQualityFromResolve(stages []diagnostic.StageDiagnostic) FactQuality {
	for _, st := range stages {
		if st.Stage == "resolve" {
			if d, ok := st.Detail.(diagnostic.ResolveDetail); ok {
				return FactQuality{Total: d.Appended}
			}
		}
	}
	return FactQuality{}
}

// SaveLatency aggregates per-stage Duration for the write trace.
func SaveLatency(trace domain.SaveTrace) time.Duration {
	var d time.Duration
	for _, st := range trace.Stages {
		d += st.Duration
	}
	return d
}
