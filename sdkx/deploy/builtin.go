package deploy

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

// Builtin after-factory kinds.
const (
	// AfterDiscardOnInterrupt builds agent.DiscardOnInterruptCauses:
	// the canonical disposition for voice / streaming UX that marks
	// Result.Committed=false when the run ends on a barge-in cause.
	AfterDiscardOnInterrupt = "discard_on_interrupt"
)

func (b *Builder) registerBuiltins() {
	b.referees[AfterDiscardOnInterrupt] = buildDiscardOnInterrupt
}

type discardSettings struct {
	Reason string   `yaml:"reason"`
	Causes []string `yaml:"causes"`
}

func buildDiscardOnInterrupt(_ context.Context, in HookInput) (agent.Referee, error) {
	s, err := DecodeSettings[discardSettings](in.Settings)
	if err != nil {
		return nil, fmt.Errorf("decode %s settings: %w", AfterDiscardOnInterrupt, err)
	}
	if len(s.Causes) == 0 {
		return nil, fmt.Errorf("%s: settings.causes must name at least one cause", AfterDiscardOnInterrupt)
	}
	causes := make([]agent.Cause, len(s.Causes))
	for i, c := range s.Causes {
		causes[i] = agent.Cause(c)
	}
	return agent.NewDiscardOnInterruptCauses(s.Reason, causes...), nil
}
