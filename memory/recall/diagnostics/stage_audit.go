package diagnostics

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

type RecallStageAudit struct {
	Stages []RecallStageSnapshot `json:"stages,omitempty"`
}

type RecallStageSnapshot struct {
	Stage             string                    `json:"stage"`
	Source            string                    `json:"source,omitempty"`
	Status            string                    `json:"status,omitempty"`
	TaskIntents       []string                  `json:"task_intents,omitempty"`
	Suggested         int                       `json:"suggested,omitempty"`
	SuggestedByTask   map[string]int            `json:"suggested_by_task,omitempty"`
	SuggestedFactIDs  []string                  `json:"suggested_fact_ids,omitempty"`
	Added             int                       `json:"added,omitempty"`
	AddedFactIDs      []string                  `json:"added_fact_ids,omitempty"`
	ScannedLinks      int                       `json:"scanned_links,omitempty"`
	AddedFacts        int                       `json:"added_facts,omitempty"`
	AddedEvidenceRefs int                       `json:"added_evidence_refs,omitempty"`
	Candidates        []RecallCandidateSnapshot `json:"candidates,omitempty"`
}

type RecallCandidateSnapshot struct {
	FactID      string   `json:"fact_id,omitempty"`
	Source      string   `json:"source,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	Score       float64  `json:"score,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Sources     []string `json:"sources,omitempty"`
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
			appendStage("context_pack", "", status, snapshotValue(d.Hits))
		case diagnostic.BuildGroundedHitsDetail:
			appendStage("build_grounded_hits", "", status, snapshotValue(d.Hits))
		}
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

func publicCandidateSnapshots(in []diagnostic.CandidateSnapshot) []RecallCandidateSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]RecallCandidateSnapshot, 0, len(in))
	for _, c := range in {
		out = append(out, RecallCandidateSnapshot{
			FactID:      c.FactID,
			Source:      c.Source,
			Rank:        c.Rank,
			Score:       c.Score,
			EvidenceIDs: append([]string(nil), c.EvidenceIDs...),
			Sources:     append([]string(nil), c.Sources...),
		})
	}
	return out
}
