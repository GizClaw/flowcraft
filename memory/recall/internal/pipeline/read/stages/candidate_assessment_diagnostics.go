package stages

import (
	"context"
	"math"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

func assessCandidate(item domain.ContextItem, intent domain.QueryIntent, state *read.ReadState) diagnostic.CandidateAssessmentComponent {
	queryText := intent.Text
	if strings.TrimSpace(queryText) == "" && state != nil {
		queryText = state.Query.Text
	}
	assessment, _ := deterministicCandidateAssessor{}.Assess(context.Background(), domain.AssessmentInput{
		QueryText: queryText,
		Intent:    intent,
		Item:      item,
		Evidence:  assessmentEvidence(item),
		Links:     assessmentItemLinks(item),
	})
	return assessmentComponent(item, assessment)
}

func assessmentComponent(item domain.ContextItem, assessment domain.CandidateAssessment) diagnostic.CandidateAssessmentComponent {
	return diagnostic.CandidateAssessmentComponent{
		ID:                 item.Candidate.ID,
		Kind:               string(item.Candidate.Kind),
		HardConstraintPass: assessment.HardConstraintPass,
		SupportScore:       assessment.SupportScore,
		StructuredScore:    assessment.StructuredScore,
		LiteralScore:       assessment.LiteralScore,
		SemanticScore:      assessment.SemanticScore,
		SourcePrior:        assessment.SourcePrior,
		RelevanceScore:     assessment.RelevanceScore,
		Confidence:         assessment.Confidence,
		Reason:             assessment.Reason,
		DropReason:         assessment.DropReason,
		FallbackReason:     assessment.FallbackReason,
		EquivalenceGroup:   assessment.EquivalenceGroup,
		SupportGroup:       assessment.SupportGroup,
		DiversityGroup:     assessment.DiversityGroup,
	}
}

func assessmentDropReasons(components []diagnostic.CandidateAssessmentComponent) map[string]int {
	if len(components) == 0 {
		return nil
	}
	out := map[string]int{}
	for _, component := range components {
		if component.DropReason == "" {
			continue
		}
		out[component.DropReason]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func assessmentScoreSummary(components []diagnostic.CandidateAssessmentComponent) diagnostic.CandidateAssessmentScoreSummary {
	if len(components) == 0 {
		return diagnostic.CandidateAssessmentScoreSummary{}
	}
	summary := diagnostic.CandidateAssessmentScoreSummary{
		Count:             len(components),
		RelevanceScoreMin: math.Inf(1),
	}
	for _, component := range components {
		if component.RelevanceScore < summary.RelevanceScoreMin {
			summary.RelevanceScoreMin = component.RelevanceScore
		}
		if component.RelevanceScore > summary.RelevanceScoreMax {
			summary.RelevanceScoreMax = component.RelevanceScore
		}
		summary.RelevanceScoreAvg += component.RelevanceScore
		summary.SemanticScoreAvg += component.SemanticScore
		summary.SupportScoreAvg += component.SupportScore
		summary.StructuredScoreAvg += component.StructuredScore
		summary.LiteralScoreAvg += component.LiteralScore
		summary.SourcePriorAvg += component.SourcePrior
		summary.ConfidenceAvg += component.Confidence
		if component.HardConstraintPass {
			summary.HardConstraintPasses++
		}
	}
	denom := float64(len(components))
	summary.RelevanceScoreAvg /= denom
	summary.SemanticScoreAvg /= denom
	summary.SupportScoreAvg /= denom
	summary.StructuredScoreAvg /= denom
	summary.LiteralScoreAvg /= denom
	summary.SourcePriorAvg /= denom
	summary.ConfidenceAvg /= denom
	return summary
}

func assessmentRejectedSnapshots(items []domain.ContextItem, components []diagnostic.CandidateAssessmentComponent) []diagnostic.CandidateSnapshot {
	if len(items) == 0 || len(components) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0)
	for i, component := range components {
		if component.DropReason == "" || i >= len(items) {
			continue
		}
		snap := candidateSnapshot(items[i].Candidate)
		snap.ScoreLabel = scoreLabelAssessment
		snap.AssessmentScore = component.RelevanceScore
		if snap.FactID == "" {
			snap.FactID = contextItemNodeID(items[i])
		}
		snap.DroppedReason = component.DropReason
		out = append(out, snap)
	}
	return out
}

func assessmentConfidence(component diagnostic.CandidateAssessmentComponent) float64 {
	if component.DropReason != "" {
		return 0
	}
	confidence := 0.25 + component.SupportScore + component.StructuredScore + component.LiteralScore + component.SemanticScore
	if confidence > 1 {
		return 1
	}
	return confidence
}

func assessmentReason(component diagnostic.CandidateAssessmentComponent) string {
	if component.DropReason != "" {
		return component.DropReason
	}
	switch {
	case component.SemanticScore > 0:
		return "semantic_support"
	case component.LiteralScore > 0 && component.SupportScore > 0:
		return "literal_exactness_with_support"
	case component.StructuredScore > 0 && component.SupportScore > 0:
		return "structured_exactness_with_support"
	case component.SupportScore > 0:
		return "evidence_supported"
	default:
		return "no_support"
	}
}
