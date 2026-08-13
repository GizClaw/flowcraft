package delegation

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	res "github.com/GizClaw/flowcraft/core/resource"
)

const (
	// ServiceKind is the deployment resource kind of the local
	// delegation service.
	ServiceKind = "delegation.Service"
	// BackendDep is the optional async backend dependency.
	BackendDep = "backend"
)

// ServiceSettings is the strict settings subtree of the local service
// factory. Zero fields leave the service defaults.
type ServiceSettings struct {
	MaxConcurrency       *int    `json:"max_concurrency,omitempty"`
	MaxDepth             *int    `json:"max_depth,omitempty"`
	Timeout              *string `json:"timeout,omitempty"`
	IdempotencyRetention *string `json:"idempotency_retention,omitempty"`
	DeferWorkers         bool    `json:"defer_workers,omitempty"`
}

type serviceFactory struct {
	directory *LocalDirectory
	options   []Option
}

// NewServiceFactory returns a deployment factory for the local
// delegation service. directory is app-owned and may be bound to a
// deploy result after assembly (it cannot be a DAG dep: it bridges to
// the deploy result itself). Options inject app-owned behavior the
// document cannot express (e.g. [delegation.WithWorkerHost]); the
// declarative settings are applied after them.
func NewServiceFactory(directory *LocalDirectory, opts ...Option) res.Factory {
	return &serviceFactory{
		directory: directory,
		options:   append([]Option(nil), opts...),
	}
}

// Spec implements res.Factory. The backend dep is optional: a service
// without one is sync-only.
func (f *serviceFactory) Spec() res.Spec {
	return res.Spec{
		Kind: ServiceKind,
		Impl: "local",
		Deps: []res.DepSpec{{
			Name: BackendDep,
			Type: "delegation.AsyncBackend",
		}},
	}
}

// New implements res.Factory.
func (f *serviceFactory) New(_ context.Context, in res.Input) (any, error) {
	if f == nil || f.directory == nil {
		return nil, errdefs.Validationf(
			"delegation service resource: directory is required")
	}
	settings, err := res.DecodeTyped[ServiceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"delegation service resource: decode settings: %w", err))
	}

	options := append([]Option(nil), f.options...)
	if settings.MaxConcurrency != nil {
		if *settings.MaxConcurrency <= 0 {
			return nil, errdefs.Validationf(
				"delegation service resource: max_concurrency must be positive")
		}
		options = append(options, WithMaxConcurrency(*settings.MaxConcurrency))
	}
	if settings.MaxDepth != nil {
		if *settings.MaxDepth <= 0 {
			return nil, errdefs.Validationf(
				"delegation service resource: max_depth must be positive")
		}
		options = append(options, WithMaxDepth(*settings.MaxDepth))
	}
	if settings.Timeout != nil {
		d, err := parseServiceDuration("timeout", *settings.Timeout)
		if err != nil {
			return nil, err
		}
		options = append(options, WithTimeout(d))
	}
	if settings.IdempotencyRetention != nil {
		d, err := parseServiceDuration("idempotency_retention", *settings.IdempotencyRetention)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, errdefs.Validationf(
				"delegation service resource: idempotency_retention must be positive")
		}
		options = append(options, WithIdempotencyRetention(d))
	}
	if settings.DeferWorkers {
		options = append(options, WithDeferredWorkers())
	}

	var backend AsyncBackend
	if value, ok := in.Dep(BackendDep); ok {
		b, ok := value.(AsyncBackend)
		if !ok || isNilBackend(b) {
			return nil, errdefs.Validationf(
				"delegation service resource: dep %q is %T, want AsyncBackend",
				BackendDep, value)
		}
		backend = b
	}
	return NewService(f.directory, backend, options...)
}

func isNilBackend(backend AsyncBackend) bool {
	if backend == nil {
		return true
	}
	value := reflect.ValueOf(backend)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func parseServiceDuration(field, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, errdefs.Validation(fmt.Errorf(
			"delegation service resource: %s: %w", field, err))
	}
	return d, nil
}
