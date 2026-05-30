package temporal

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func sampleFact(id, mergeKey string, kind domain.FactKind, ts time.Time, entities ...string) domain.TemporalFact {
	return domain.TemporalFact{
		ID:         id,
		Scope:      scope(),
		Kind:       kind,
		Content:    "c-" + id,
		MergeKey:   mergeKey,
		Entities:   entities,
		ObservedAt: ts,
	}
}
