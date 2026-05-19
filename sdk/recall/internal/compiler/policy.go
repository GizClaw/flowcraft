package compiler

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// Policy is the governance hook on the write path. It can mutate or
// reject a fact (privacy / retention / consent / user-delete). PR-2
// ships a no-op so the boundary is real but never blocks Save.
type Policy interface {
	Apply(f model.TemporalFact) (model.TemporalFact, bool)
}

type noopPolicy struct{}

func (noopPolicy) Apply(f model.TemporalFact) (model.TemporalFact, bool) {
	return f, true
}
