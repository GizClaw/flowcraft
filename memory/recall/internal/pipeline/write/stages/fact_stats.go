package stages

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// computeFactStats walks a fact slice and tallies per-fact shape
// signals (content / structured-only / evidence / valid_from /
// confidence / kind). Owned by the stages package because
// diagnostic/ cannot import domain/ (cycle): the stage holds both
// imports and precomputes the tally before emitting Detail.
//
// The three "shape" buckets — WithContent, StructuredOnly,
// EmptyRenderable — partition Total. WithEvidence / WithValidFrom /
// WithConfidence are independent presence flags. ByKind buckets by
// FactKind so operators see drift by kind without a second walk.
func computeFactStats(facts []domain.TemporalFact) diagnostic.FactStats {
	stats := diagnostic.FactStats{Total: len(facts)}
	if len(facts) == 0 {
		return stats
	}
	stats.ByKind = make(map[string]int, 6)
	for _, f := range facts {
		hasContent := strings.TrimSpace(f.Content) != ""
		hasStructured := strings.TrimSpace(f.Subject) != "" ||
			strings.TrimSpace(f.Predicate) != "" ||
			strings.TrimSpace(f.Object) != ""
		hasEvidence := strings.TrimSpace(f.EvidenceText) != "" || anyEvidenceRefText(f.EvidenceRefs)
		switch {
		case hasContent:
			stats.WithContent++
		case hasStructured:
			stats.StructuredOnly++
		case !hasEvidence:
			stats.EmptyRenderable++
		}
		if hasEvidence {
			stats.WithEvidence++
		}
		if f.ValidFrom != nil {
			stats.WithValidFrom++
		}
		if f.Confidence > 0 {
			stats.WithConfidence++
		}
		stats.ByKind[string(f.Kind)]++
	}
	return stats
}

func anyEvidenceRefText(refs []domain.EvidenceRef) bool {
	for _, r := range refs {
		if strings.TrimSpace(r.Text) != "" {
			return true
		}
	}
	return false
}
