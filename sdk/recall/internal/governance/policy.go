package governance

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// WritePolicy is the governance hook on the write path (docs §10.2).
// It may mutate or reject a fact (privacy / retention / consent).
// The default implementation never blocks Save.
type WritePolicy interface {
	Apply(f model.TemporalFact) (model.TemporalFact, bool)
}

// NopWritePolicy allows every fact through unchanged.
type NopWritePolicy struct{}

func (NopWritePolicy) Apply(f model.TemporalFact) (model.TemporalFact, bool) {
	return f, true
}
