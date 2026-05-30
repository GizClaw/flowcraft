package governance

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain"

// NopWritePolicy allows every fact through unchanged.
type NopWritePolicy struct{}

func (NopWritePolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) {
	return f, true
}
