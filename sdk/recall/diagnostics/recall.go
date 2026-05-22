// Package diagnostics is the only public diagnostics surface for
// sdk/recall (Phase E.2 / E.3). Every function is a pure consumer of
// trace.Stages — the package never reaches into ranker / fusion /
// materialize implementations, and only depends on internal/domain
// (Stages-only) and internal/domain/diagnostic (per-stage details).
package diagnostics

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// SourceDiagnostic describes one CandidateSource's contribution.
type SourceDiagnostic struct {
	Source    string
	Activated bool
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// HitRenderability reports whether returned hits carry renderable text.
type HitRenderability struct {
	Total            int
	EmptyRenderable  int
	StructuredOnly   int
	GroundedEvidence int
	EmptyTop         int
}

// HitProvenance reports how winners were attributed across sources.
type HitProvenance struct {
	WinnersBySource    map[string]int
	SoleSourceWinners  map[string]int
	MultiSourceWinners int
	NoProvenance       int
}

// RecallDiagnostics summarises per-stage health of one Recall call.
type RecallDiagnostics struct {
	Plan             diagnostic.PlanView
	Sources          []SourceDiagnostic
	Drops            []diagnostic.CandidateDrop
	FusedCandidates  int
	Materialized     int
	DropsByStage     map[FailureStage]int
	HitRenderability HitRenderability
	HitProvenance    HitProvenance
	TotalLatency     time.Duration
	Attributions     []Attribution
}

// DiagnoseRecall produces a per-stage health view from trace.Stages.
// Hits are inspected for renderability + provenance attribution; pass
// nil when only the trace-derived view is needed.
func DiagnoseRecall(trace domain.RecallTrace, hits []domain.Hit) RecallDiagnostics {
	stages := trace.Stages
	out := RecallDiagnostics{
		Plan:            diagnostic.ExtractPlan(stages),
		Sources:         Sources(trace),
		Drops:           Drops(trace),
		FusedCandidates: diagnostic.ExtractFusedCandidates(stages),
		Materialized:    diagnostic.ExtractMaterialized(stages),
		TotalLatency:    totalLatency(stages),
		Attributions:    AttributeRecallTrace(trace),
	}
	if len(out.Drops) > 0 {
		out.DropsByStage = make(map[FailureStage]int, len(out.Drops))
		for _, d := range out.Drops {
			out.DropsByStage[StageFromDropReason(d.Reason)]++
		}
	}
	out.HitRenderability = diagnoseHits(hits)
	out.HitProvenance = diagnoseHitProvenance(hits)
	return out
}

// Plan is the planner view reconstructed from trace.Stages.
func Plan(trace domain.RecallTrace) diagnostic.PlanView {
	return diagnostic.ExtractPlan(trace.Stages)
}

// Sources reconstructs per-source contributions from trace.Stages.
func Sources(trace domain.RecallTrace) []SourceDiagnostic {
	views := diagnostic.ExtractSources(trace.Stages)
	out := make([]SourceDiagnostic, len(views))
	for i, v := range views {
		out[i] = SourceDiagnostic{
			Source:    v.Source,
			Activated: v.Activated,
			Budget:    v.Budget,
			Returned:  v.Returned,
			Truncated: v.Truncated,
			Latency:   v.Latency,
			Err:       v.Err,
		}
	}
	return out
}

// Drops returns read-path candidate drops from trace.Stages.
func Drops(trace domain.RecallTrace) []diagnostic.CandidateDrop {
	return append([]diagnostic.CandidateDrop(nil), diagnostic.ExtractDrops(trace.Stages)...)
}

// Materialized returns the materialized count from trace.Stages.
func Materialized(trace domain.RecallTrace) int {
	return diagnostic.ExtractMaterialized(trace.Stages)
}

// FusedCandidates returns the fused pool size from trace.Stages.
func FusedCandidates(trace domain.RecallTrace) int {
	return diagnostic.ExtractFusedCandidates(trace.Stages)
}

func totalLatency(stages []diagnostic.StageDiagnostic) time.Duration {
	var d time.Duration
	for _, st := range stages {
		d += st.Duration
	}
	return d
}

func diagnoseHits(hits []domain.Hit) HitRenderability {
	out := HitRenderability{Total: len(hits)}
	const topK = 3
	for i, h := range hits {
		content := strings.TrimSpace(h.Fact.Content)
		structured := h.Fact.Subject != "" || h.Fact.Predicate != "" || h.Fact.Object != ""
		evidence := strings.TrimSpace(h.Fact.EvidenceText) != "" || len(hitEvidenceRefs(h)) > 0
		if evidence {
			out.GroundedEvidence++
		}
		if content != "" {
			continue
		}
		if structured {
			out.StructuredOnly++
			continue
		}
		out.EmptyRenderable++
		if i < topK {
			out.EmptyTop++
		}
	}
	return out
}

func diagnoseHitProvenance(hits []domain.Hit) HitProvenance {
	prov := HitProvenance{
		WinnersBySource:   map[string]int{},
		SoleSourceWinners: map[string]int{},
	}
	for _, h := range hits {
		if len(h.Sources) == 0 {
			prov.NoProvenance++
			continue
		}
		for _, src := range h.Sources {
			prov.WinnersBySource[src]++
		}
		if len(h.Sources) == 1 {
			prov.SoleSourceWinners[h.Sources[0]]++
		} else {
			prov.MultiSourceWinners++
		}
	}
	return prov
}

// AnswerContextItem is one rendered memory item passed to an answer stage.
type AnswerContextItem struct {
	FactID string
	Text   string
}

// AttributeAnswerContext detects evidence-grounded facts whose
// rendered context dropped the grounding (regression attribution
// between Recall and the caller's answer stage).
func AttributeAnswerContext(hits []domain.Hit, rendered []AnswerContextItem) []Attribution {
	if len(hits) == 0 || len(rendered) == 0 {
		return nil
	}
	byID := make(map[string]string, len(rendered))
	for _, r := range rendered {
		byID[r.FactID] = r.Text
	}
	var out []Attribution
	for _, h := range hits {
		text, ok := byID[h.Fact.ID]
		if !ok {
			continue
		}
		evid := strings.TrimSpace(h.Fact.EvidenceText)
		if evid == "" && len(h.Fact.EvidenceRefs) == 0 {
			continue
		}
		if evid != "" && strings.Contains(text, evid) {
			continue
		}
		grounded := false
		for _, ref := range hitEvidenceRefs(h) {
			t := strings.TrimSpace(ref.Text)
			if t == "" {
				continue
			}
			if strings.Contains(text, t) {
				grounded = true
				break
			}
		}
		if grounded {
			continue
		}
		out = append(out, Attribution{
			Stage:   FailureAnswer,
			FactID:  h.Fact.ID,
			Reason:  "evidence_grounding_not_rendered",
			Details: text,
		})
	}
	return out
}

func hitEvidenceRefs(h domain.Hit) []domain.EvidenceRef {
	if len(h.Evidence) > 0 {
		return h.Evidence
	}
	return h.Fact.EvidenceRefs
}
