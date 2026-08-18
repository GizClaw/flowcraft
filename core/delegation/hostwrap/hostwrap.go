package hostwrap

import (
	"context"
	"reflect"

	"github.com/GizClaw/flowcraft/core/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// Deployment is the minimal read-only deployment view needed to locate a
// delegation service. *deploy.Result implements it.
type Deployment interface {
	Names() []string
	Value(name string) (any, bool)
}

// Wrap exposes the single delegation.Service found in deployment on every
// host created by hostFactory via delegation.WithService. Multiple services
// are rejected; a deployment without a service leaves the factory unchanged.
//
// The caller owns both factory and deployment; Wrap only borrows them. The
// canonical wiring is a runtime result-aware host factory decorator:
//
//	builder.WithResultHostFactory(func(result *deploy.Result,
//		factory session.HostFactory) (session.HostFactory, error) {
//		return hostwrap.Wrap(factory, result)
//	})
func Wrap(hostFactory session.HostFactory, deployment Deployment) (session.HostFactory, error) {
	if isNilInterface(hostFactory) {
		return nil, errdefs.Validationf("delegation hostwrap: nil host factory")
	}
	if isNilInterface(deployment) {
		return nil, errdefs.Validationf("delegation hostwrap: nil deployment")
	}
	var service sdkdelegation.Service
	for _, name := range deployment.Names() {
		value, ok := deployment.Value(name)
		if !ok {
			continue
		}
		if candidate, ok := value.(sdkdelegation.Service); ok && !isNilInterface(candidate) {
			if !isNilInterface(service) {
				return nil, errdefs.Conflictf(
					"delegation hostwrap: multiple delegation services built (%s)", name)
			}
			service = candidate
		}
	}
	if isNilInterface(service) {
		return hostFactory, nil
	}
	return serviceHostFactory{inner: hostFactory, service: service}, nil
}

type serviceHostFactory struct {
	inner   session.HostFactory
	service sdkdelegation.Service
}

func (f serviceHostFactory) NewHost(ctx context.Context, request session.HostRequest) (agent.Host, error) {
	if isNilInterface(f.inner) {
		return nil, errdefs.Internalf("delegation hostwrap: nil inner host factory")
	}
	host, err := f.inner.NewHost(ctx, request)
	if err != nil {
		return nil, err
	}
	if isNilInterface(host) {
		return nil, errdefs.Internalf("delegation hostwrap: inner host factory returned nil host")
	}
	return sdkdelegation.WithService(host, f.service), nil
}

var _ session.HostFactory = serviceHostFactory{}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
