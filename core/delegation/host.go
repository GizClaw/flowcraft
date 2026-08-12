package delegation

import (
	"reflect"

	"github.com/GizClaw/flowcraft/core/agent"
)

// ServiceProvider is the optional Host capability exposing delegation.
type ServiceProvider interface {
	DelegationService() Service
}

// WithService wraps h with a ServiceProvider capability. Install this wrapper
// before agent Host middleware so built-in decorators can preserve access
// through agent.CapabilityFromHost.
func WithService(h agent.Host, service Service) agent.Host {
	if nilInterface(h) {
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
	if nilInterface(service) {
		return nil, false
	}
	return service, true
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

var _ ServiceProvider = serviceHost{}
var _ agent.HostUnwrapper = serviceHost{}
