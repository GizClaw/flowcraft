package session

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// StartOption configures one StartWithOptions / ResumeWithOptions call.
// Existing callers of Start / Resume never see these options.
type StartOption func(*startConfig) error

type startConfig struct {
	sinks        []SinkSpec
	askUser      AskUserFunc
	ephemeral    bool
	ephemeralSet bool
}

// WithSinks attaches stream sinks, mirroring the variadic sinks of
// Session.Start for the option-based entry point.
func WithSinks(sinks ...SinkSpec) StartOption {
	return func(config *startConfig) error {
		config.sinks = append(config.sinks, sinks...)
		return nil
	}
}

// WithAskUserOverride replaces the turn's default PromptRequested-based
// asker. The override is invoked directly by the turn host without
// publishing prompt lifecycle events or blocking for a reply.
func WithAskUserOverride(askUser AskUserFunc) StartOption {
	return func(config *startConfig) error {
		if isNil(askUser) {
			return errdefs.Validationf(
				"runtime session: AskUser override must not be nil")
		}
		config.askUser = askUser
		return nil
	}
}

// WithEphemeral marks the session ephemeral (fixed by the first Start):
// no session-state or run-checkpoint writes, no history seeding, canceled
// turns never park, and Resume reports not found. Mixing ephemeral and
// persistent starts on the same session is rejected.
func WithEphemeral() StartOption {
	return func(config *startConfig) error {
		config.ephemeral = true
		config.ephemeralSet = true
		return nil
	}
}

func applyStartOptions(options []StartOption) (startConfig, error) {
	config := startConfig{}
	for _, option := range options {
		if isNil(option) {
			return startConfig{}, errdefs.Validationf(
				"runtime session: StartOption must not be nil")
		}
		if err := option(&config); err != nil {
			return startConfig{}, err
		}
	}
	return config, nil
}

// ephemeralHost wraps a turn Host so run checkpoints are never written:
// ephemeral sessions must not leave any durable trace in the checkpoint
// store.
type ephemeralHost struct {
	agent.Host
}

func (ephemeralHost) Checkpoint(context.Context, agent.Checkpoint) error {
	return nil
}

var _ agent.Checkpointer = ephemeralHost{}
