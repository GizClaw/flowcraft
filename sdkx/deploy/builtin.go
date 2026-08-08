package deploy

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
)

// Builtin referee factory kinds.
const (
	// AfterDiscardOnInterrupt builds agent.DiscardOnInterruptCauses:
	// the canonical disposition for voice / streaming UX that marks
	// Result.Committed=false when the run ends on a barge-in cause.
	AfterDiscardOnInterrupt = "discard_on_interrupt"
)

func (b *Builder) registerBuiltins() {
	b.MustRegisterFactory(discardOnInterruptFactory{})
}

type discardSettings struct {
	Reason string   `json:"reason"`
	Causes []string `json:"causes"`
}

// discardOnInterruptFactory implements config.Factory for the built-in
// discard-on-interrupt referee.
type discardOnInterruptFactory struct{}

// Spec implements config.Factory.
func (discardOnInterruptFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: HookKindReferee, Impl: AfterDiscardOnInterrupt}
}

// New implements config.Factory: settings.causes names the interrupt
// causes that discard the committed result.
func (discardOnInterruptFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	s, err := sdkconfig.DecodeSettings[discardSettings](in.Settings)
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
