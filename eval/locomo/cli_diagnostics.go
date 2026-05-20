package locomo

import (
	"encoding/json"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

// diagnosticsReport is the JSON shape written by --diagnostics. It is a
// flattened view of recall.PipelineHealth with the few percentages we
// actually care about precomputed, so the file is human-skimmable.
type diagnosticsReport struct {
	GeneratedAt   time.Time `json:"generated_at"`
	SaveSamples   int       `json:"save_samples"`
	RecallSamples int       `json:"recall_samples"`
	InputFacts    int       `json:"input_facts"`
	// InputCoverage breaks the Save inputs down by channel and
	// typed-field coverage. The ratios (e.g. typed_time_pct) are
	// the single most attributable signal for adapter regressions:
	// if temporal-question accuracy drops, this is the first place
	// to look — was Time actually populated, or did the adapter
	// silently fall back to grep-prose mode?
	InputCoverage inputCoverageReport `json:"input_coverage"`
	// StructurizerCoverage exposes how often each Structurizer
	// sub-task (kind / entities / subject / valid_from) actually
	// fired across the run. The pct fields are the ratio over
	// total_facts_seen so operators can read "kind keyword fallback
	// owned X% of classifications" at a glance.
	StructurizerCoverage structurizerCoverageReport `json:"structurizer_coverage"`
	Compiled             factQualityReport          `json:"compiled_facts"`
	Appended             factQualityReport          `json:"appended_facts"`
	SaveDrops            map[string]int             `json:"save_drops,omitempty"`
	HitRenderability     hitReport                  `json:"hit_renderability"`
	RecallDrops          map[string]int             `json:"recall_drops,omitempty"`
	RecallLatencyAvg     string                     `json:"recall_latency_avg,omitempty"`
	SourceActivation     map[string]int             `json:"source_activation,omitempty"`
	SourceReturnedAv     map[string]int             `json:"source_returned_avg,omitempty"`
	// Provenance: which sources actually surfaced final winners. The
	// distinction between "source activated" and "source produced a
	// final hit" is the answer to "is this projection pulling its
	// weight" — a source can run 235 times and still contribute 0
	// final winners if its candidates always lose fusion ranking.
	WinnersBySource    map[string]int `json:"winners_by_source,omitempty"`
	SoleSourceWinners  map[string]int `json:"sole_source_winners,omitempty"`
	MultiSourceWinners int            `json:"multi_source_winners,omitempty"`
	NoProvenanceHits   int            `json:"hits_no_provenance,omitempty"`
}

type factQualityReport struct {
	Total              int            `json:"total"`
	WithContent        int            `json:"with_content"`
	WithContentPct     float64        `json:"with_content_pct"`
	StructuredOnly     int            `json:"structured_only"`
	StructuredOnlyPct  float64        `json:"structured_only_pct"`
	EmptyRenderable    int            `json:"empty_renderable"`
	EmptyRenderablePct float64        `json:"empty_renderable_pct"`
	WithEvidence       int            `json:"with_evidence"`
	WithEvidencePct    float64        `json:"with_evidence_pct"`
	WithValidFrom      int            `json:"with_valid_from"`
	WithConfidence     int            `json:"with_confidence"`
	ByKind             map[string]int `json:"by_kind,omitempty"`
}

// inputCoverageReport is the JSON-friendly view of recall.InputCoverage,
// with precomputed coverage ratios so report consumers can sort /
// alert without re-computing.
type inputCoverageReport struct {
	Facts               int     `json:"facts"`
	Turns               int     `json:"turns"`
	TurnsWithTypedTime  int     `json:"turns_with_typed_time"`
	TurnsTypedTimePct   float64 `json:"turns_typed_time_pct"`
	TurnsWithSpeaker    int     `json:"turns_with_speaker"`
	TurnsSpeakerPct     float64 `json:"turns_speaker_pct"`
	TurnsWithEvidenceID int     `json:"turns_with_evidence_id"`
	TurnsEvidenceIDPct  float64 `json:"turns_evidence_id_pct"`
	TurnsWithSessionID  int     `json:"turns_with_session_id"`
	KnownEntitiesTotal  int     `json:"known_entities_total"`
	KnownEntitiesAvg    int     `json:"known_entities_avg"`
	SavesWithObservedAt int     `json:"saves_with_observed_at"`
	SavesObservedAtPct  float64 `json:"saves_observed_at_pct"`
}

// structurizerCoverageReport is the JSON-friendly view of
// recall.StructurizerCoverage with precomputed fill-rate ratios.
type structurizerCoverageReport struct {
	TotalFactsSeen         int     `json:"total_facts_seen"`
	KindFilled             int     `json:"kind_filled"`
	KindFilledPct          float64 `json:"kind_filled_pct"`
	EntitiesFilled         int     `json:"entities_filled"`
	EntitiesFilledPct      float64 `json:"entities_filled_pct"`
	SubjectFilled          int     `json:"subject_filled"`
	SubjectFilledPct       float64 `json:"subject_filled_pct"`
	ValidFromHintFilled    int     `json:"valid_from_hint_filled"`
	ValidFromHintFilledPct float64 `json:"valid_from_hint_filled_pct"`
}

type hitReport struct {
	Total              int     `json:"total"`
	EmptyRenderable    int     `json:"empty_renderable"`
	EmptyRenderablePct float64 `json:"empty_renderable_pct"`
	StructuredOnly     int     `json:"structured_only"`
	GroundedEvidence   int     `json:"grounded_evidence"`
	GroundedPct        float64 `json:"grounded_evidence_pct"`
	EmptyTop           int     `json:"empty_top"`
}

func writeDiagnosticsReport(path string, h *recall.PipelineHealth) error {
	report := diagnosticsReport{
		GeneratedAt:          time.Now(),
		SaveSamples:          h.SaveSamples,
		RecallSamples:        h.RecallSamples,
		InputFacts:           h.InputFacts,
		InputCoverage:        toInputCoverageReport(h),
		StructurizerCoverage: toStructurizerCoverageReport(h.StructurizerCoverage),
		Compiled:             toFactQualityReport(h.CompiledFacts),
		Appended:             toFactQualityReport(h.AppendedFacts),
		HitRenderability:     toHitReport(h.HitRenderability),
		SourceActivation:     h.SourceActivation,
		WinnersBySource:      h.WinnersBySource,
		SoleSourceWinners:    h.SoleSourceWinners,
		MultiSourceWinners:   h.MultiSourceWinners,
		NoProvenanceHits:     h.NoProvenanceHits,
	}
	if len(h.SaveDrops) > 0 {
		report.SaveDrops = make(map[string]int, len(h.SaveDrops))
		for stage, n := range h.SaveDrops {
			report.SaveDrops[string(stage)] = n
		}
	}
	if len(h.RecallDrops) > 0 {
		report.RecallDrops = make(map[string]int, len(h.RecallDrops))
		for stage, n := range h.RecallDrops {
			report.RecallDrops[string(stage)] = n
		}
	}
	if h.RecallSamples > 0 {
		avg := h.RecallLatency / time.Duration(h.RecallSamples)
		report.RecallLatencyAvg = avg.String()
		if len(h.SourceReturned) > 0 {
			report.SourceReturnedAv = make(map[string]int, len(h.SourceReturned))
			for src, sum := range h.SourceReturned {
				count := h.SourceActivation[src]
				if count == 0 {
					continue
				}
				report.SourceReturnedAv[src] = sum / count
			}
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func toStructurizerCoverageReport(c recall.StructurizerCoverage) structurizerCoverageReport {
	r := structurizerCoverageReport{
		TotalFactsSeen:      c.TotalFactsSeen,
		KindFilled:          c.KindFilled,
		EntitiesFilled:      c.EntitiesFilled,
		SubjectFilled:       c.SubjectFilled,
		ValidFromHintFilled: c.ValidFromHintFilled,
	}
	if c.TotalFactsSeen > 0 {
		r.KindFilledPct = ratio(c.KindFilled, c.TotalFactsSeen)
		r.EntitiesFilledPct = ratio(c.EntitiesFilled, c.TotalFactsSeen)
		r.SubjectFilledPct = ratio(c.SubjectFilled, c.TotalFactsSeen)
		r.ValidFromHintFilledPct = ratio(c.ValidFromHintFilled, c.TotalFactsSeen)
	}
	return r
}

func toFactQualityReport(q recall.FactQuality) factQualityReport {
	r := factQualityReport{
		Total:           q.Total,
		WithContent:     q.WithContent,
		StructuredOnly:  q.StructuredOnly,
		EmptyRenderable: q.EmptyRenderable,
		WithEvidence:    q.WithEvidence,
		WithValidFrom:   q.WithValidFrom,
		WithConfidence:  q.WithConfidence,
		ByKind:          q.ByKind,
	}
	if q.Total > 0 {
		r.WithContentPct = ratio(q.WithContent, q.Total)
		r.StructuredOnlyPct = ratio(q.StructuredOnly, q.Total)
		r.EmptyRenderablePct = ratio(q.EmptyRenderable, q.Total)
		r.WithEvidencePct = ratio(q.WithEvidence, q.Total)
	}
	return r
}

func toHitReport(h recall.HitRenderability) hitReport {
	r := hitReport{
		Total:            h.Total,
		EmptyRenderable:  h.EmptyRenderable,
		StructuredOnly:   h.StructuredOnly,
		GroundedEvidence: h.GroundedEvidence,
		EmptyTop:         h.EmptyTop,
	}
	if h.Total > 0 {
		r.EmptyRenderablePct = ratio(h.EmptyRenderable, h.Total)
		r.GroundedPct = ratio(h.GroundedEvidence, h.Total)
	}
	return r
}

// toInputCoverageReport flattens recall.InputCoverage and the
// related PipelineHealth counters into the JSON-friendly form,
// pre-dividing the typed-field coverage against the Turns base so
// dashboards / alerts can sort by ratio directly. KnownEntitiesAvg
// is the per-Save average (integer, since fractional snapshots
// don't mean anything).
func toInputCoverageReport(h *recall.PipelineHealth) inputCoverageReport {
	cov := h.InputCoverage
	r := inputCoverageReport{
		Facts:               cov.Facts,
		Turns:               cov.Turns,
		TurnsWithTypedTime:  cov.TurnsWithTypedTime,
		TurnsWithSpeaker:    cov.TurnsWithSpeaker,
		TurnsWithEvidenceID: cov.TurnsWithEvidenceID,
		TurnsWithSessionID:  cov.TurnsWithSessionID,
		KnownEntitiesTotal:  cov.KnownEntities,
		SavesWithObservedAt: h.SavesWithObservedAt,
	}
	if cov.Turns > 0 {
		r.TurnsTypedTimePct = ratio(cov.TurnsWithTypedTime, cov.Turns)
		r.TurnsSpeakerPct = ratio(cov.TurnsWithSpeaker, cov.Turns)
		r.TurnsEvidenceIDPct = ratio(cov.TurnsWithEvidenceID, cov.Turns)
	}
	if h.SaveSamples > 0 {
		r.KnownEntitiesAvg = cov.KnownEntities / h.SaveSamples
		r.SavesObservedAtPct = ratio(h.SavesWithObservedAt, h.SaveSamples)
	}
	return r
}

func ratio(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) / float64(denom)
}
