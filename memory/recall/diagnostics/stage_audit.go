package diagnostics

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

type RecallStageAudit struct {
	Stages []RecallStageSnapshot `json:"stages,omitempty"`
}

type RecallStageSnapshot struct {
	Stage             string                        `json:"stage"`
	Source            string                        `json:"source,omitempty"`
	Status            string                        `json:"status,omitempty"`
	Query             *RecallQueryIntent            `json:"query_intent,omitempty"`
	ActivatedLenses   []RecallActivatedLens         `json:"activated_lenses,omitempty"`
	TaskIntents       []string                      `json:"task_intents,omitempty"`
	TotalBudget       int                           `json:"total_budget,omitempty"`
	Suggested         int                           `json:"suggested,omitempty"`
	SuggestedByTask   map[string]int                `json:"suggested_by_task,omitempty"`
	SuggestedFactIDs  []string                      `json:"suggested_fact_ids,omitempty"`
	InputCount        int                           `json:"input_count,omitempty"`
	OutputCount       int                           `json:"output_count,omitempty"`
	Dropped           int                           `json:"dropped,omitempty"`
	DropReasons       map[string]int                `json:"drop_reasons,omitempty"`
	Added             int                           `json:"added,omitempty"`
	AddedFactIDs      []string                      `json:"added_fact_ids,omitempty"`
	ScannedLinks      int                           `json:"scanned_links,omitempty"`
	AddedFacts        int                           `json:"added_facts,omitempty"`
	AddedEvidenceRefs int                           `json:"added_evidence_refs,omitempty"`
	CoverageBundles   []RecallCoverageBundle        `json:"coverage_bundles,omitempty"`
	ScoreSummary      *RecallAssessmentScoreSummary `json:"score_summary,omitempty"`
	Candidates        []RecallCandidateSnapshot     `json:"candidates,omitempty"`
	Assessment        []RecallAssessmentComponent   `json:"assessment,omitempty"`
	PackTrace         []RecallCandidateSnapshot     `json:"pack_trace,omitempty"`
}

type RecallQueryIntent struct {
	QueryLen                      int                          `json:"query_len,omitempty"`
	Entities                      []string                     `json:"entities,omitempty"`
	Kinds                         []string                     `json:"kinds,omitempty"`
	Subject                       string                       `json:"subject,omitempty"`
	Predicate                     string                       `json:"predicate,omitempty"`
	Object                        string                       `json:"object,omitempty"`
	HasTimeRange                  bool                         `json:"has_time_range,omitempty"`
	HasExplicitDate               bool                         `json:"has_explicit_date,omitempty"`
	HasRelativeTemporalExpression bool                         `json:"has_relative_temporal_expression,omitempty"`
	TokenCount                    int                          `json:"token_count,omitempty"`
	NumericCount                  int                          `json:"numeric_count,omitempty"`
	QuotedCount                   int                          `json:"quoted_count,omitempty"`
	ProperCount                   int                          `json:"proper_count,omitempty"`
	Strategy                      string                       `json:"strategy,omitempty"`
	Confidence                    float64                      `json:"confidence,omitempty"`
	Alternates                    []RecallIntentRouteCandidate `json:"alternates,omitempty"`
	Signals                       []string                     `json:"signals,omitempty"`
	FallbackReason                string                       `json:"fallback_reason,omitempty"`
}

type RecallIntentRouteCandidate struct {
	Strategy   string  `json:"strategy,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type RecallActivatedLens struct {
	Lens        string  `json:"lens,omitempty"`
	Weight      float64 `json:"weight,omitempty"`
	Budget      int     `json:"budget,omitempty"`
	ActivatedBy string  `json:"activated_by,omitempty"`
}

type RecallCoverageBundle struct {
	SeedFactID      string   `json:"seed_fact_id,omitempty"`
	RescuedFactIDs  []string `json:"rescued_fact_ids,omitempty"`
	ReplacedFactIDs []string `json:"replaced_fact_ids,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

type RecallCandidateSnapshot struct {
	FactID           string   `json:"fact_id,omitempty"`
	Source           string   `json:"source,omitempty"`
	Rank             int      `json:"rank,omitempty"`
	ScoreLabel       string   `json:"score_label,omitempty"`
	DiscoveryScore   float64  `json:"discovery_score,omitempty"`
	AssessmentScore  float64  `json:"assessment_relevance_score,omitempty"`
	RankScore        float64  `json:"rank_score,omitempty"`
	FinalScore       float64  `json:"final_score,omitempty"`
	EvidenceIDs      []string `json:"evidence_ids,omitempty"`
	Sources          []string `json:"sources,omitempty"`
	RankOutputRank   int      `json:"rank_output_rank,omitempty"`
	ContextPackRank  int      `json:"context_pack_rank,omitempty"`
	PrimarySource    string   `json:"primary_source,omitempty"`
	ProjectionRoutes []string `json:"projection_routes,omitempty"`
	DroppedReason    string   `json:"dropped_reason,omitempty"`
}

type RecallAssessmentComponent struct {
	ID                 string  `json:"id,omitempty"`
	Kind               string  `json:"kind,omitempty"`
	HardConstraintPass bool    `json:"hard_constraint_pass,omitempty"`
	SupportScore       float64 `json:"support_score,omitempty"`
	StructuredScore    float64 `json:"structured_score,omitempty"`
	LiteralScore       float64 `json:"literal_score,omitempty"`
	SemanticScore      float64 `json:"semantic_score,omitempty"`
	SourcePrior        float64 `json:"source_prior,omitempty"`
	RelevanceScore     float64 `json:"relevance_score,omitempty"`
	Confidence         float64 `json:"confidence,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	DropReason         string  `json:"drop_reason,omitempty"`
	FallbackReason     string  `json:"fallback_reason,omitempty"`
	EquivalenceGroup   string  `json:"equivalence_group,omitempty"`
	SupportGroup       string  `json:"support_group,omitempty"`
	DiversityGroup     string  `json:"diversity_group,omitempty"`
}

type RecallAssessmentScoreSummary struct {
	Count                int     `json:"count,omitempty"`
	RelevanceScoreMin    float64 `json:"relevance_score_min,omitempty"`
	RelevanceScoreMax    float64 `json:"relevance_score_max,omitempty"`
	RelevanceScoreAvg    float64 `json:"relevance_score_avg,omitempty"`
	SemanticScoreAvg     float64 `json:"semantic_score_avg,omitempty"`
	SupportScoreAvg      float64 `json:"support_score_avg,omitempty"`
	StructuredScoreAvg   float64 `json:"structured_score_avg,omitempty"`
	LiteralScoreAvg      float64 `json:"literal_score_avg,omitempty"`
	SourcePriorAvg       float64 `json:"source_prior_avg,omitempty"`
	ConfidenceAvg        float64 `json:"confidence_avg,omitempty"`
	HardConstraintPasses int     `json:"hard_constraint_passes,omitempty"`
}

func AuditRecallStages(trace domain.RecallTrace) RecallStageAudit {
	var out RecallStageAudit
	appendStage := func(stage, source, status string, snaps []diagnostic.CandidateSnapshot) {
		out.Stages = append(out.Stages, RecallStageSnapshot{
			Stage:      stage,
			Source:     source,
			Status:     status,
			Candidates: publicCandidateSnapshots(snaps),
		})
	}
	for _, st := range trace.Stages {
		status := string(st.Status)
		switch d := st.Detail.(type) {
		case diagnostic.IntentRouteDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:  "intent_route",
				Status: status,
				Query:  publicQueryIntent(d),
			})
		case diagnostic.PlanDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:           "plan",
				Status:          status,
				TaskIntents:     append([]string(nil), d.TaskIntents...),
				TotalBudget:     d.TotalBudget,
				ActivatedLenses: publicActivatedLenses(d.ActivatedLenses),
			})
		case diagnostic.CandidateFanoutDetail:
			for _, src := range d.Sources {
				appendStage("candidate_fanout", src.Lens, status, snapshotValue(src.Snapshots))
			}
		case diagnostic.CandidateMergeAndMaterializeDetail:
			appendStage("candidate_merge", "", status, snapshotValue(d.CandidateSnapshots))
			appendStage("candidate_materialize", "", status, snapshotValue(d.MaterializedSnapshots))
			appendStage("candidate_merge_and_materialize", "", status, snapshotValue(d.Output))
		case diagnostic.CandidateExpansionDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:            "candidate_expansion",
				Status:           status,
				TaskIntents:      append([]string(nil), d.TaskIntents...),
				Added:            d.Added,
				AddedFactIDs:     append([]string(nil), d.AddedFactIDs...),
				Suggested:        d.Suggested,
				SuggestedByTask:  cloneIntMap(d.SuggestedByTask),
				SuggestedFactIDs: append([]string(nil), d.SuggestedFactIDs...),
				Candidates:       publicCandidateSnapshots(snapshotValue(d.Items)),
			})
		case diagnostic.ObservationRecallDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:        "observation_recall",
				Status:       status,
				Added:        d.AddedObservations,
				AddedFactIDs: append([]string(nil), d.AddedObservationIDs...),
				Candidates:   publicCandidateSnapshots(snapshotValue(d.Items)),
			})
		case diagnostic.LinkExpansionDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:             "link_expansion",
				Status:            status,
				Added:             d.AddedFacts,
				AddedFactIDs:      append([]string(nil), d.AddedFactIDs...),
				ScannedLinks:      d.ScannedLinks,
				AddedFacts:        d.AddedFacts,
				AddedEvidenceRefs: d.AddedEvidenceRefs,
				Candidates:        publicCandidateSnapshots(snapshotValue(d.Items)),
			})
		case diagnostic.PolicyFilterDetail:
			appendStage("policy_filter", "", status, snapshotValue(d.Items))
		case diagnostic.CandidateAssessmentDetail:
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:        "candidate_assessment",
				Status:       status,
				InputCount:   d.InputCount,
				OutputCount:  d.OutputCount,
				Dropped:      d.Dropped,
				DropReasons:  cloneIntMap(d.DropReasons),
				ScoreSummary: publicAssessmentScoreSummary(d.ScoreSummary),
				Candidates:   publicCandidateSnapshots(snapshotValue(d.Items)),
				Assessment:   publicAssessmentComponents(d.Components),
			})
		case diagnostic.RankDetail:
			appendStage("rank_input", "", status, snapshotValue(d.Input))
			appendStage("rank_output", "", status, snapshotValue(d.Output))
		case diagnostic.ContextPackDetail:
			if d.Input != nil {
				appendStage("context_pack_input", "", status, snapshotValue(d.Input))
			}
			if d.RerankedHits != nil {
				appendStage("context_pack_reranked", "", status, snapshotValue(d.RerankedHits))
			}
			out.Stages = append(out.Stages, RecallStageSnapshot{
				Stage:           "context_pack",
				Status:          status,
				CoverageBundles: publicCoverageBundles(d.CoverageBundles),
				Candidates:      publicCandidateSnapshots(snapshotValue(d.Hits)),
				PackTrace:       publicCandidateSnapshots(snapshotValue(d.PackTrace)),
			})
		case diagnostic.BuildGroundedHitsDetail:
			appendStage("build_grounded_hits", "", status, snapshotValue(d.Hits))
		}
	}
	return out
}

func publicQueryIntent(d diagnostic.IntentRouteDetail) *RecallQueryIntent {
	return &RecallQueryIntent{
		QueryLen:                      d.QueryLen,
		Entities:                      append([]string(nil), d.Entities...),
		Kinds:                         append([]string(nil), d.Kinds...),
		Subject:                       d.Subject,
		Predicate:                     d.Predicate,
		Object:                        d.Object,
		HasTimeRange:                  d.HasTimeRange,
		HasExplicitDate:               d.HasExplicitDate,
		HasRelativeTemporalExpression: d.HasRelativeTemporalExpression,
		TokenCount:                    d.TokenCount,
		NumericCount:                  d.NumericCount,
		QuotedCount:                   d.QuotedCount,
		ProperCount:                   d.ProperCount,
		Strategy:                      d.Strategy,
		Confidence:                    d.Confidence,
		Alternates:                    publicIntentRouteCandidates(d.Alternates),
		Signals:                       append([]string(nil), d.Signals...),
		FallbackReason:                d.FallbackReason,
	}
}

func publicIntentRouteCandidates(in []diagnostic.IntentRouteCandidate) []RecallIntentRouteCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallIntentRouteCandidate, 0, len(in))
	for _, candidate := range in {
		out = append(out, RecallIntentRouteCandidate{
			Strategy:   candidate.Strategy,
			Confidence: candidate.Confidence,
		})
	}
	return out
}

func publicActivatedLenses(in []diagnostic.ActivatedLens) []RecallActivatedLens {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallActivatedLens, 0, len(in))
	for _, lens := range in {
		out = append(out, RecallActivatedLens{
			Lens:        lens.Lens,
			Weight:      lens.Weight,
			Budget:      lens.Budget,
			ActivatedBy: lens.ActivatedBy,
		})
	}
	return out
}

func publicCoverageBundles(in []diagnostic.CoverageBundle) []RecallCoverageBundle {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallCoverageBundle, 0, len(in))
	for _, bundle := range in {
		out = append(out, RecallCoverageBundle{
			SeedFactID:      bundle.SeedFactID,
			RescuedFactIDs:  append([]string(nil), bundle.RescuedFactIDs...),
			ReplacedFactIDs: append([]string(nil), bundle.ReplacedFactIDs...),
			Reason:          bundle.Reason,
		})
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func snapshotValue(in *[]diagnostic.CandidateSnapshot) []diagnostic.CandidateSnapshot {
	if in == nil {
		return nil
	}
	return *in
}

func publicAssessmentComponents(in []diagnostic.CandidateAssessmentComponent) []RecallAssessmentComponent {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallAssessmentComponent, 0, len(in))
	for _, component := range in {
		out = append(out, RecallAssessmentComponent{
			ID:                 component.ID,
			Kind:               component.Kind,
			HardConstraintPass: component.HardConstraintPass,
			SupportScore:       component.SupportScore,
			StructuredScore:    component.StructuredScore,
			LiteralScore:       component.LiteralScore,
			SemanticScore:      component.SemanticScore,
			SourcePrior:        component.SourcePrior,
			RelevanceScore:     component.RelevanceScore,
			Confidence:         component.Confidence,
			Reason:             component.Reason,
			DropReason:         component.DropReason,
			FallbackReason:     component.FallbackReason,
			EquivalenceGroup:   component.EquivalenceGroup,
			SupportGroup:       component.SupportGroup,
			DiversityGroup:     component.DiversityGroup,
		})
	}
	return out
}

func publicAssessmentScoreSummary(in diagnostic.CandidateAssessmentScoreSummary) *RecallAssessmentScoreSummary {
	if in.Count == 0 {
		return nil
	}
	return &RecallAssessmentScoreSummary{
		Count:                in.Count,
		RelevanceScoreMin:    in.RelevanceScoreMin,
		RelevanceScoreMax:    in.RelevanceScoreMax,
		RelevanceScoreAvg:    in.RelevanceScoreAvg,
		SemanticScoreAvg:     in.SemanticScoreAvg,
		SupportScoreAvg:      in.SupportScoreAvg,
		StructuredScoreAvg:   in.StructuredScoreAvg,
		LiteralScoreAvg:      in.LiteralScoreAvg,
		SourcePriorAvg:       in.SourcePriorAvg,
		ConfidenceAvg:        in.ConfidenceAvg,
		HardConstraintPasses: in.HardConstraintPasses,
	}
}

func publicCandidateSnapshots(in []diagnostic.CandidateSnapshot) []RecallCandidateSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallCandidateSnapshot, 0, len(in))
	for _, c := range in {
		out = append(out, RecallCandidateSnapshot{
			FactID:           c.FactID,
			Source:           c.Source,
			Rank:             c.Rank,
			ScoreLabel:       c.ScoreLabel,
			DiscoveryScore:   c.DiscoveryScore,
			AssessmentScore:  c.AssessmentScore,
			RankScore:        c.RankScore,
			FinalScore:       c.FinalScore,
			EvidenceIDs:      append([]string(nil), c.EvidenceIDs...),
			Sources:          append([]string(nil), c.Sources...),
			RankOutputRank:   c.RankOutputRank,
			ContextPackRank:  c.ContextPackRank,
			PrimarySource:    c.PrimarySource,
			ProjectionRoutes: append([]string(nil), c.ProjectionRoutes...),
			DroppedReason:    c.DroppedReason,
		})
	}
	return out
}
