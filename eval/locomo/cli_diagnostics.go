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
	GeneratedAt      time.Time         `json:"generated_at"`
	SaveSamples      int               `json:"save_samples"`
	RecallSamples    int               `json:"recall_samples"`
	InputFacts       int               `json:"input_facts"`
	Compiled         factQualityReport `json:"compiled_facts"`
	Appended         factQualityReport `json:"appended_facts"`
	SaveDrops        map[string]int    `json:"save_drops,omitempty"`
	HitRenderability hitReport         `json:"hit_renderability"`
	RecallDrops      map[string]int    `json:"recall_drops,omitempty"`
	RecallLatencyAvg string            `json:"recall_latency_avg,omitempty"`
	SourceActivation map[string]int    `json:"source_activation,omitempty"`
	SourceReturnedAv map[string]int    `json:"source_returned_avg,omitempty"`
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
		GeneratedAt:        time.Now(),
		SaveSamples:        h.SaveSamples,
		RecallSamples:      h.RecallSamples,
		InputFacts:         h.InputFacts,
		Compiled:           toFactQualityReport(h.CompiledFacts),
		Appended:           toFactQualityReport(h.AppendedFacts),
		HitRenderability:   toHitReport(h.HitRenderability),
		SourceActivation:   h.SourceActivation,
		WinnersBySource:    h.WinnersBySource,
		SoleSourceWinners:  h.SoleSourceWinners,
		MultiSourceWinners: h.MultiSourceWinners,
		NoProvenanceHits:   h.NoProvenanceHits,
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

func ratio(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) / float64(denom)
}
