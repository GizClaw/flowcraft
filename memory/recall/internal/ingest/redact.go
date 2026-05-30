package ingest

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// RedactDroppedFacts strips fact payloads from drops while retaining
// stable identifiers for operator dashboards (FactID, Kind, content
// hash). Full TemporalFact bodies are omitted from default telemetry.
func RedactDroppedFacts(drops []diagnostic.DroppedFact) []diagnostic.DroppedFact {
	if len(drops) == 0 {
		return nil
	}
	out := make([]diagnostic.DroppedFact, len(drops))
	for i, d := range drops {
		out[i] = diagnostic.DroppedFact{Reason: d.Reason}
		f, ok := d.Fact.(domain.TemporalFact)
		if !ok {
			continue
		}
		out[i].FactID = f.ID
		out[i].Kind = string(f.Kind)
		if f.Content != "" {
			out[i].ContentHash = contentHashHex(f.Content)
		}
	}
	return out
}

func contentHashHex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}
