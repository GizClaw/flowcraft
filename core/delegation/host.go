package delegation

import (
	"reflect"

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

// inheritedHost returns the Host a delegated run inherits from the
// caller's context. A caller that exposes a steer source — a runtime
// turn host — keeps it: the queue is turn-scoped, and a delegated
// document draining it would consume corrections addressed to the
// caller's own board. Everything else stays reachable through
// traversal, so the child still delegates (ServiceProvider) and
// streams (EventBusProvider). Hosts without a steer source pass
// through unchanged, and the child then holds the caller's own Host
// value — the historical propagation behavior.
func inheritedHost(host agent.Host) agent.Host {
	if _, ok := agent.SteerFromHost(host); !ok {
		return host
	}
	return steerlessHost{Host: host}
}

// steerlessHost withholds the inherited [agent.SteerSource] from the
// delegated run. It deliberately keeps unwrapping: the mask names one
// capability instead of replacing the authorising surface, so a
// subagent nested in the delegated run reads the same delegation
// service and event bus the caller would.
type steerlessHost struct {
	agent.Host
}

// UnwrapHost keeps every other capability of the inherited Host
// reachable.
func (h steerlessHost) UnwrapHost() agent.Host { return h.Host }

// MaskedCapability implements [agent.HostCapabilityMask].
func (h steerlessHost) MaskedCapability() reflect.Type {
	return reflect.TypeFor[agent.SteerSource]()
}

var _ ServiceProvider = serviceHost{}
var _ agent.HostUnwrapper = serviceHost{}
var _ agent.HostUnwrapper = steerlessHost{}
var _ agent.HostCapabilityMask = steerlessHost{}
