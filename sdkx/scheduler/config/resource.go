// Package config exposes scheduler servers as deployment resources.
package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/scheduler"
)

const (
	// ResourceKind is the deploy resource kind implemented by scheduler servers.
	ResourceKind = "scheduler.Server"
	// LocalImpl is the in-process scheduler server implementation.
	LocalImpl = "local"
)

type localFactory struct{}

// NewLocalDeployFactory builds one unstarted process-local scheduler Server.
// Runtime starts it only after every integration has mounted its tasks.
func NewLocalDeployFactory() deploy.ResourceFactory {
	return localFactory{}
}

func (localFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{Kind: ResourceKind, Impl: LocalImpl}
}

func (localFactory) New(_ context.Context, input deploy.ResourceInput) (any, error) {
	if _, err := deploy.DecodeSettings[struct{}](input.Settings); err != nil {
		return nil, errdefs.Validation(err)
	}
	return scheduler.NewLocalServer()
}
