package entityfact

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/derive"
)

// NoopExtractor satisfies the entity fact capability without deriving facts.
type NoopExtractor struct{}

var _ derive.EntityFactExtractor = NoopExtractor{}

func (NoopExtractor) ExtractEntityFacts(context.Context, derive.EntityFactInput) (derive.EntityFactOutput, error) {
	return derive.EntityFactOutput{}, nil
}
