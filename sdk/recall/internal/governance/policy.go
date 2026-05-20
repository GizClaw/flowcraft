package governance

import "github.com/GizClaw/flowcraft/sdk/recall/internal/domain"

// WritePolicy is the governance hook on the write path (docs §10.2).
// It may mutate or reject a fact (privacy / retention / consent).
// The default implementation never blocks Save.
type WritePolicy interface {
	Apply(f domain.TemporalFact) (domain.TemporalFact, bool)
}

// NopWritePolicy allows every fact through unchanged.
type NopWritePolicy struct{}

func (NopWritePolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) {
	return f, true
}
