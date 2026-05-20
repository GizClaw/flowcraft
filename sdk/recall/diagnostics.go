package recall

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

// FailureStage is the diagnostics taxonomy used for recall attribution.
type FailureStage = telemetry.FailureStage

const (
	FailureExtract     = telemetry.StageExtract
	FailureNormalize   = telemetry.StageNormalize
	FailureEntity      = telemetry.StageEntity
	FailureTime        = telemetry.StageTime
	FailureMerge       = telemetry.StageMerge
	FailureProjection  = telemetry.StageProjection
	FailureSource      = telemetry.StageSource
	FailureFusion      = telemetry.StageFusion
	FailureMaterialize = telemetry.StageMaterialize
	FailureRerank      = telemetry.StageRerank
	FailureAnswer      = telemetry.StageAnswer
)

// FailureAttribution records one pipeline failure observation. Phase 8
// does not auto-repair; use RepairPlan for operator-driven fixes.
type FailureAttribution = telemetry.Attribution

// DroppedFact carries a public write-path drop reason for attribution.
type DroppedFact struct {
	Fact   TemporalFact
	Reason string
}

// AnswerContextItem is one rendered memory item that was passed to an
// answer synthesizer after Recall. It lets diagnostics compare the
// canonical hit with the actual text a downstream answer stage saw.
type AnswerContextItem struct {
	FactID string
	Text   string
}

// RepairPlan lists fact ids suitable for ProjectionRebuilder.RepairStale.
type RepairPlan = evolution.RepairPlan

// AttributeRecallTrace maps a RecallExplain trace to failure stages.
func AttributeRecallTrace(trace RecallTrace) []FailureAttribution {
	return telemetry.AttributeRecallTrace(trace)
}

// AttributeSaveDrops maps compiler drops from Save to failure stages.
func AttributeSaveDrops(dropped []DroppedFact) []FailureAttribution {
	if len(dropped) == 0 {
		return nil
	}
	td := make([]telemetry.DroppedFact, len(dropped))
	for i, d := range dropped {
		td[i] = telemetry.DroppedFact{Fact: d.Fact, Reason: d.Reason}
	}
	return telemetry.AttributeDroppedFacts(td)
}

// AttributeAnswerContext detects grounding lost between Recall and the
// answer stage. A hit may carry source evidence on TemporalFact, but if
// the downstream context renderer only passes Fact.Content, the answer
// model cannot use exact dates, wording, or source details that recall
// found. This helper attributes that as an answer-stage miss, not a
// source/fusion/materialize failure.
func AttributeAnswerContext(hits []Hit, rendered []AnswerContextItem) []FailureAttribution {
	if len(hits) == 0 {
		return nil
	}
	byID := make(map[string]string, len(rendered))
	for _, item := range rendered {
		if item.FactID != "" {
			byID[item.FactID] = item.Text
		}
	}
	var out []FailureAttribution
	for i, hit := range hits {
		renderedText := ""
		if hit.Fact.ID != "" {
			renderedText = byID[hit.Fact.ID]
		}
		if renderedText == "" && i < len(rendered) {
			renderedText = rendered[i].Text
		}
		if renderedText == "" {
			continue
		}
		missing := missingGrounding(hit.Fact, renderedText)
		if len(missing) == 0 {
			continue
		}
		out = append(out, FailureAttribution{
			Stage:   FailureAnswer,
			FactID:  hit.Fact.ID,
			Reason:  "evidence_grounding_not_rendered",
			Details: strings.Join(missing, "; "),
		})
	}
	return out
}

// RepairPlanFromTrace derives a projection repair plan from drops.
func RepairPlanFromTrace(scope Scope, trace RecallTrace) RepairPlan {
	return evolution.PlanFromRecallTrace(scope, trace)
}

// RepairPlanFromDrifts derives a repair plan from drift telemetry.
func RepairPlanFromDrifts(scope Scope, drifts []DriftEvent) RepairPlan {
	return evolution.PlanFromDrifts(scope, drifts)
}

// StageFromPipeline maps pipeline stage names to failure stages.
func StageFromPipeline(stage string) FailureStage {
	return telemetry.StageFromPipeline(stage)
}

// StageFromDropReason maps CandidateDrop reasons to failure stages.
func StageFromDropReason(reason DropReason) FailureStage {
	return telemetry.StageFromDropReason(reason)
}

// SourceDiagnostic describes one CandidateSource's contribution to a
// single Recall. It is derived from RecallTrace; the Activated flag
// distinguishes "the planner picked this source but it produced 0
// candidates" from "the planner did not pick it at all".
type SourceDiagnostic struct {
	Source    string
	Activated bool
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// HitRenderability reports whether returned Hits carry enough text for
// an answer renderer to ground on. EmptyRenderable means the canonical
// fact has no Content / Subject-Predicate-Object / Evidence text — the
// hit is effectively unusable downstream and should be treated as a
// projection or extractor bug rather than a recall miss.
type HitRenderability struct {
	Total            int
	EmptyRenderable  int
	StructuredOnly   int
	GroundedEvidence int
	EmptyTop         int
}

// HitProvenance reports how the final Recall winners were attributed
// across the configured CandidateSources. WinnersBySource counts how
// many Hits carried each source in their provenance (multi-source
// hits add to every contributing source's bucket). SoleSourceWinners
// counts Hits surfaced by exactly one source — useful for spotting a
// source that is the only path to certain facts. MultiSourceWinners
// counts Hits found by ≥ 2 sources (high-confidence corroboration).
// NoProvenance counts Hits whose provenance metadata is missing
// (should be zero on the canonical read path; > 0 indicates a bug).
type HitProvenance struct {
	WinnersBySource    map[string]int
	SoleSourceWinners  map[string]int
	MultiSourceWinners int
	NoProvenance       int
}

// RecallDiagnostics summarises the per-stage health of a single Recall
// call. Unlike AttributeRecallTrace (which only emits records when a
// stage failed), RecallDiagnostics reports the shape of every stage so
// operators can answer "why is recall quality low" directly from the
// SDK trace + hits without exporting JSONL and grepping.
type RecallDiagnostics struct {
	Plan             QueryPlan
	Sources          []SourceDiagnostic
	FusedCandidates  int
	Materialized     int
	DropsByStage     map[FailureStage]int
	HitRenderability HitRenderability
	HitProvenance    HitProvenance
	TotalLatency     time.Duration
	Attributions     []FailureAttribution
}

// DiagnoseRecall produces a per-stage health view of a Recall call. It
// is safe to call with hits == nil (Recall returned trace only) or
// trace == RecallTrace{} (Recall returned hits only); fields that
// cannot be computed stay at their zero value.
func DiagnoseRecall(trace RecallTrace, hits []Hit) RecallDiagnostics {
	out := RecallDiagnostics{
		Plan:            trace.Plan,
		FusedCandidates: trace.FusedCandidates,
		Materialized:    trace.Materialized,
		TotalLatency:    trace.TotalLatency,
		Attributions:    AttributeRecallTrace(trace),
	}
	out.Sources = diagnoseSources(trace)
	if len(trace.Drops) > 0 {
		out.DropsByStage = make(map[FailureStage]int, len(trace.Drops))
		for _, d := range trace.Drops {
			out.DropsByStage[StageFromDropReason(d.Reason)]++
		}
	}
	out.HitRenderability = diagnoseHits(hits)
	out.HitProvenance = diagnoseHitProvenance(hits)
	return out
}

func diagnoseHitProvenance(hits []Hit) HitProvenance {
	out := HitProvenance{
		WinnersBySource:   map[string]int{},
		SoleSourceWinners: map[string]int{},
	}
	for _, h := range hits {
		if len(h.Sources) == 0 {
			out.NoProvenance++
			continue
		}
		uniq := map[string]struct{}{}
		for _, src := range h.Sources {
			if src == "" {
				continue
			}
			if _, dup := uniq[src]; dup {
				continue
			}
			uniq[src] = struct{}{}
			out.WinnersBySource[src]++
		}
		if len(uniq) == 1 {
			for src := range uniq {
				out.SoleSourceWinners[src]++
			}
		} else if len(uniq) >= 2 {
			out.MultiSourceWinners++
		}
	}
	return out
}

func diagnoseSources(trace RecallTrace) []SourceDiagnostic {
	seen := make(map[string]bool, len(trace.Sources))
	out := make([]SourceDiagnostic, 0, len(trace.Plan.SourceOrder)+len(trace.Sources))
	for _, st := range trace.Sources {
		seen[st.Source] = true
		out = append(out, SourceDiagnostic{
			Source:    st.Source,
			Activated: true,
			Budget:    st.Budget,
			Returned:  st.Returned,
			Truncated: st.Truncated,
			Latency:   st.Latency,
			Err:       st.Err,
		})
	}
	for _, src := range trace.Plan.SourceOrder {
		if seen[src] {
			continue
		}
		out = append(out, SourceDiagnostic{
			Source: src,
			Budget: trace.Plan.SourceBudgets[src],
		})
	}
	return out
}

func diagnoseHits(hits []Hit) HitRenderability {
	out := HitRenderability{Total: len(hits)}
	if len(hits) == 0 {
		return out
	}
	for i, h := range hits {
		hasContent := strings.TrimSpace(h.Fact.Content) != ""
		hasStructured := strings.TrimSpace(h.Fact.Subject) != "" ||
			strings.TrimSpace(h.Fact.Predicate) != "" ||
			strings.TrimSpace(h.Fact.Object) != ""
		hasEvidence := strings.TrimSpace(h.Fact.EvidenceText) != "" || anyRefText(h.Fact.EvidenceRefs)
		if !hasContent && !hasStructured && !hasEvidence {
			out.EmptyRenderable++
			if i < 3 {
				out.EmptyTop++
			}
			continue
		}
		if !hasContent && hasStructured {
			out.StructuredOnly++
		}
		if hasEvidence {
			out.GroundedEvidence++
		}
	}
	return out
}

// FactQuality summarises the shape of compiled facts produced by a
// single Save call. It quantifies what the extractor / compiler stack
// actually produces: how many facts carry usable Content, how many
// are structured-only (S/P/O without Content), how many bring
// grounding evidence, and how the kinds break down. Operators read
// this to answer "is the extractor returning fact bodies the answer
// LLM can ground on" without having to grep traces.
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

// InputCoverage quantifies what the adapter actually fed the SDK
// through SaveRequest. The typed channel (Turns with Time, Speaker,
// EvidenceID set) is what unlocks deterministic Structurizer fills
// downstream; an adapter that forgot to populate it silently
// regresses the LLM to grep-prose mode without surfacing an error.
// Operators read InputCoverage to answer the questions error logs
// cannot: "did the adapter even attempt to ground time / speaker
// for this call?", "is the entity-projection seeding the
// canonicalisation hint?", "did the caller anchor a wall clock for
// replay?".
type InputCoverage struct {
	// Facts is the count of caller-supplied structured facts. The
	// passthrough channel; usually 0 for LLM-driven ingest and
	// non-zero for migration / rule-based pipelines.
	Facts int
	// Turns is the count of TurnContexts whose Text is non-empty
	// (= what the LLM extractor sees). Empty Turns means the
	// extractor was skipped.
	Turns int
	// TurnsWithTypedTime counts turns where Time is non-zero. The
	// Structurizer uses Time verbatim for ValidFrom hints; without
	// it the extractor falls back to regex-grepping content for
	// dates, which costs accuracy on temporal questions.
	TurnsWithTypedTime int
	// TurnsWithSpeaker counts turns where Speaker is non-empty.
	// Used to lift fact Subject from a canonical name instead of
	// the role token ("user" / "assistant"), so multi-speaker
	// conversations don't collapse onto a single subject.
	TurnsWithSpeaker int
	// TurnsWithEvidenceID counts turns where the adapter supplied
	// a benchmark-specific evidence id. recall.k_hit can only
	// score citations when this is set.
	TurnsWithEvidenceID int
	// TurnsWithSessionID counts turns that carry a session bucket
	// (e.g. "session_3"). Session-aware sources / batching only
	// fire when this is populated.
	TurnsWithSessionID int
	// KnownEntities is the number of canonical entity snapshots
	// the SDK lifted from the entity projection at Compile time.
	// 0 on the very first Save in a scope; rising thereafter. A
	// suspiciously flat value across many Saves indicates the
	// projection isn't accumulating mentions.
	KnownEntities int
	// HasObservedAt reports whether the caller anchored a wall
	// clock for relative-time resolution. False = the compiler
	// falls back to time.Now(), which is incorrect for historical
	// replay (evals, backfill).
	HasObservedAt bool
}

// SaveDiagnostics is the per-stage health view of a single Save call.
// It pairs compiler drops (already covered by AttributeSaveDrops) with
// quality metrics on what survived. Both numbers matter for accuracy:
// a high drop rate hides facts; a low drop rate but mostly empty
// content hides the same facts in a different way.
type SaveDiagnostics struct {
	// Input is the total number of input items (facts + non-empty
	// turns). Headline count for quick "did anything reach the
	// compiler" reads.
	Input int
	// InputCoverage breaks Input down by channel and by typed-
	// field coverage so operators can attribute extractor
	// regressions to a missing input signal vs. an LLM regression.
	InputCoverage InputCoverage
	// StructurizerCoverage breaks the compiler's Structurizer stage
	// down by sub-task. Operators read this to attribute accuracy
	// shifts to a specific responsibility (Kind / Entities /
	// Subject / ValidFrom) before reaching for a refactor — e.g.
	// if KindFilled stays at 0 across many runs, the keyword
	// fallback is dead code and the LLM enum owns classification.
	StructurizerCoverage StructurizerCoverage
	Compiled             FactQuality
	Appended             FactQuality
	DropsByStage         map[FailureStage]int
	Attributions         []FailureAttribution
}

// DiagnoseSave produces a per-stage health view of a Save call. It is
// safe to call when trace.Appended is empty (the resolver may have
// deduped everything) or trace.CompiledFacts is empty (the extractor
// returned no facts). Both branches are first-class signals.
func DiagnoseSave(req SaveRequest, trace SaveTrace) SaveDiagnostics {
	cov := inputCoverage(req, trace)
	out := SaveDiagnostics{
		Input:                cov.Facts + cov.Turns,
		InputCoverage:        cov,
		StructurizerCoverage: trace.StructurizerCoverage,
		Compiled:             factQuality(trace.CompiledFacts),
		Appended:             factQuality(trace.Appended),
	}
	if len(trace.Dropped) > 0 {
		out.Attributions = AttributeSaveDrops(trace.Dropped)
		out.DropsByStage = make(map[FailureStage]int, len(out.Attributions))
		for _, a := range out.Attributions {
			out.DropsByStage[a.Stage]++
		}
	}
	return out
}

// inputCoverage walks the SaveRequest once and counts what the
// adapter actually populated. We count Turns once for "non-empty
// Text" (the inclusion criterion for the extractor) and then sub-
// count Time / Speaker / EvidenceID / SessionID against that base
// so the ratios are meaningful (e.g. TurnsWithTypedTime / Turns =
// temporal-grounding coverage).
func inputCoverage(req SaveRequest, trace SaveTrace) InputCoverage {
	cov := InputCoverage{
		Facts:         len(req.Facts),
		KnownEntities: trace.KnownEntitiesSeen,
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

func factQuality(facts []TemporalFact) FactQuality {
	q := FactQuality{Total: len(facts)}
	if len(facts) == 0 {
		return q
	}
	q.ByKind = make(map[string]int)
	for _, f := range facts {
		hasContent := strings.TrimSpace(f.Content) != ""
		hasStructured := strings.TrimSpace(f.Subject) != "" ||
			strings.TrimSpace(f.Predicate) != "" ||
			strings.TrimSpace(f.Object) != ""
		hasEvidence := strings.TrimSpace(f.EvidenceText) != "" || anyRefText(f.EvidenceRefs)
		if hasContent {
			q.WithContent++
		}
		if !hasContent && hasStructured {
			q.StructuredOnly++
		}
		if !hasContent && !hasStructured && !hasEvidence {
			q.EmptyRenderable++
		}
		if hasEvidence {
			q.WithEvidence++
		}
		if f.ValidFrom != nil {
			q.WithValidFrom++
		}
		if f.Confidence > 0 {
			q.WithConfidence++
		}
		q.ByKind[string(f.Kind)]++
	}
	return q
}

// PipelineHealth aggregates per-stage health over many Save and Recall
// calls. Use it to answer "across the whole workload, what percentage
// of facts arrive at the answer stage empty" without running a
// separate analytics pipeline.
type PipelineHealth struct {
	SaveSamples   int
	RecallSamples int
	InputFacts    int
	// InputCoverage is the summed coverage across every Save call.
	// Read ratios (e.g. TurnsWithTypedTime / Turns) to spot
	// adapter regressions: if temporal-question accuracy dropped,
	// check whether the adapter is still populating Time.
	InputCoverage       InputCoverage
	SavesWithObservedAt int
	// StructurizerCoverage is the summed per-stage Structurizer
	// coverage across every Save call. Read ratios (e.g.
	// KindFilled / TotalFactsSeen) to see how much of the
	// Structurizer's nominal 4-job bundle is actually firing in
	// practice — a stage that ratios to 0 across many Saves is a
	// candidate for deletion or replacement.
	StructurizerCoverage StructurizerCoverage
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

// mergeStructurizerCoverage sums per-Save Structurizer coverage
// counters into the aggregate so PipelineHealth exposes total
// fill-rates over the whole workload, not just a single call.
func mergeStructurizerCoverage(dst *StructurizerCoverage, src StructurizerCoverage) {
	dst.TotalFactsSeen += src.TotalFactsSeen
	dst.KindFilled += src.KindFilled
	dst.EntitiesFilled += src.EntitiesFilled
	dst.SubjectFilled += src.SubjectFilled
	dst.ValidFromHintFilled += src.ValidFromHintFilled
}

// mergeInputCoverage sums per-Save coverage counters into the
// aggregate. HasObservedAt is a boolean per call — we track it on
// PipelineHealth.SavesWithObservedAt instead, so on the aggregate
// the field stays false (a sum-of-bools would be meaningless).
func mergeInputCoverage(dst *InputCoverage, src InputCoverage) {
	dst.Facts += src.Facts
	dst.Turns += src.Turns
	dst.TurnsWithTypedTime += src.TurnsWithTypedTime
	dst.TurnsWithSpeaker += src.TurnsWithSpeaker
	dst.TurnsWithEvidenceID += src.TurnsWithEvidenceID
	dst.TurnsWithSessionID += src.TurnsWithSessionID
	dst.KnownEntities += src.KnownEntities
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

func anyRefText(refs []EvidenceRef) bool {
	for _, r := range refs {
		if strings.TrimSpace(r.Text) != "" {
			return true
		}
	}
	return false
}

func missingGrounding(f TemporalFact, rendered string) []string {
	rendered = normalizeDiagnosticText(rendered)
	var missing []string
	check := func(label, text string) {
		text = normalizeDiagnosticText(text)
		if text == "" || text == rendered || strings.Contains(rendered, text) {
			return
		}
		missing = append(missing, label)
	}
	check("evidence_text", f.EvidenceText)
	for _, ref := range f.EvidenceRefs {
		check("evidence_ref:"+firstNonEmpty(ref.ID, ref.MessageID), ref.Text)
	}
	return missing
}

func normalizeDiagnosticText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "unknown"
}
