// Package config adapts delegation decision hooks to sdkx/deploy factories.
package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
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
func NewHandoffRefereeFactory(directory sdkdelegation.Directory) deploy.RefereeFactory {
	return func(_ context.Context, in deploy.HookInput) (agent.Referee, error) {
		if _, err := deploy.DecodeSettings[struct{}](in.Settings); err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"delegation config: decode %s referee settings: %w",
				RefereeType, err))
		}
		if directory == nil {
			return nil, errdefs.Validationf(
				"delegation config: %s referee directory is nil",
				RefereeType)
		}
		return sdkdelegation.DirectoryHandoffReferee(directory), nil
	}
}
