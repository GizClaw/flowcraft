package diagnostics

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

type RecallStageAudit struct {
	Stages []RecallStageSnapshot `json:"stages,omitempty"`
}

type RecallStageSnapshot struct {
	Stage      string                    `json:"stage"`
	Source     string                    `json:"source,omitempty"`
	Status     string                    `json:"status,omitempty"`
	Candidates []RecallCandidateSnapshot `json:"candidates,omitempty"`
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
		case diagnostic.FederationFanoutDetail:
			for _, src := range d.Sources {
				appendStage("source_fanout", src.Lens, status, snapshotValue(src.Snapshots))
			}
			appendStage("fusion", "", status, snapshotValue(d.Fused))
			appendStage("materialize", "", status, snapshotValue(d.MaterializedItems))
		case diagnostic.FederationMergeDetail:
			appendStage("federation_merge", "", status, snapshotValue(d.Items))
		case diagnostic.TrustFilterDetail:
			appendStage("trust_filter", "", status, snapshotValue(d.Items))
		case diagnostic.RankDetail:
			appendStage("rank_input", "", status, snapshotValue(d.Input))
			appendStage("rank_output", "", status, snapshotValue(d.Output))
		case diagnostic.BuildHitsDetail:
			appendStage("build_hits", "", status, snapshotValue(d.Hits))
		}
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
