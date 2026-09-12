package delegation

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// ServiceProvider is the optional Host capability exposing delegation.
type ServiceProvider interface {
	DelegationService() Service
}

// WithService wraps h with a ServiceProvider capability. Install this wrapper
// before agent Host middleware so built-in decorators can preserve access
// through agent.CapabilityFromHost.
func WithService(h agent.Host, service Service) agent.Host {
	if ptr.IsNil(h) {
		panic("delegation.WithService: Host is nil")
	}
	return serviceHost{Host: h, service: service}
}

type serviceHost struct {
	agent.Host
	service Service
}

func (h serviceHost) DelegationService() Service { return h.service }

// UnwrapHost preserves optional capabilities exposed by the wrapped Host.
func (h serviceHost) UnwrapHost() agent.Host { return h.Host }

// ServiceFromHost returns the delegation service borrowed from h.
func ServiceFromHost(h agent.Host) (Service, bool) {
	provider, ok := agent.CapabilityFromHost[ServiceProvider](h)
	if !ok {
		return nil, false
	}
	service := provider.DelegationService()
	if ptr.IsNil(service) {
		return nil, false
	}
	return service, true
}

var _ ServiceProvider = serviceHost{}
var _ agent.HostUnwrapper = serviceHost{}
