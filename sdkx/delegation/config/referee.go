// Package config adapts delegation decision hooks to sdkx/deploy factories.
package config

import (
	"context"
	"fmt"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

const (
	// RefereeType is the agent.referees type for dynamic handoff detection.
	RefereeType = "delegation_handoff"
)

// NewHandoffRefereeFactory returns a deploy referee factory that captures
// directory without reading it. The directory may therefore be bound after
// deployment assembly and before the referee's After method runs.
func NewHandoffRefereeFactory(directory sdkdelegation.Directory) sdkconfig.Factory {
	return &handoffRefereeFactory{directory: directory}
}

// handoffRefereeFactory implements config.Factory for the dynamic
// handoff-detection referee.
type handoffRefereeFactory struct {
	directory sdkdelegation.Directory
}

// Spec implements config.Factory.
func (handoffRefereeFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: deploy.HookKindReferee, Impl: RefereeType}
}

// New implements config.Factory: the referee takes no settings.
func (f *handoffRefereeFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	if _, err := sdkconfig.DecodeSettings[struct{}](in.Settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"delegation config: decode %s referee settings: %w",
			RefereeType, err))
	}
	if f.directory == nil {
		return nil, errdefs.Validationf(
			"delegation config: %s referee directory is nil",
			RefereeType)
	}
	return sdkdelegation.DirectoryHandoffReferee(f.directory), nil
}
