package session

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

// HostFactory constructs a fresh Host for one turn.
type HostFactory interface {
	NewHost(context.Context, HostRequest) (agent.Host, error)
}

// HostFactoryFunc adapts a function to HostFactory.
type HostFactoryFunc func(context.Context, HostRequest) (agent.Host, error)

// NewHost implements HostFactory.
func (f HostFactoryFunc) NewHost(ctx context.Context, request HostRequest) (agent.Host, error) {
	return f(ctx, request)
}
