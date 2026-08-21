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
	// DirectoryKind is the deployment resource kind of the local
	// delegation directory.
	DirectoryKind = "delegation.Directory"
	// SessionProviderKind is the deployment resource kind of a delegation
	// session identity policy.
	SessionProviderKind = "delegation.SessionProvider"
	// BackendDep is the optional async backend dependency.
	BackendDep = "backend"
	// DirectoryDep is the required directory dependency shared by the
	// service and the delegation tool source.
	DirectoryDep = "directory"
	// SessionProviderDep is the optional session identity policy
	// dependency.
	SessionProviderDep = "session_provider"
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
	options []Option
}

// NewServiceFactory returns a deployment factory for the local
// delegation service. The service resolves its directory and optional
// session provider from deploy dependencies, so every deployment
// generation gets its own bound directory. Options inject app-owned
// behavior the document cannot express (e.g. [delegation.WithWorkerHost]);
// the declarative settings are applied after them.
func NewServiceFactory(opts ...Option) res.Factory {
	return &serviceFactory{
		options: append([]Option(nil), opts...),
	}
}

// Spec implements res.Factory. The backend dep is optional: a service
// without one is sync-only. The directory dep is required and binds the
// assembled deployment per generation.
func (f *serviceFactory) Spec() res.Spec {
	return res.Spec{
		Kind: ServiceKind,
		Impl: "local",
		Deps: []res.DepSpec{{
			Name: BackendDep,
			Type: "delegation.AsyncBackend",
		}, {
			Name:     DirectoryDep,
			Type:     "delegation.Directory",
			Required: true,
		}, {
			Name: SessionProviderDep,
			Type: "delegation.SessionProvider",
		}},
	}
}

// New implements res.Factory.
func (f *serviceFactory) New(_ context.Context, in res.Input) (any, error) {
	if f == nil {
		return nil, errdefs.Validationf(
			"delegation service resource: nil factory")
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

	var directory *LocalDirectory
	if value, ok := in.Dep(DirectoryDep); ok {
		dir, ok := value.(*LocalDirectory)
		if !ok || dir == nil {
			return nil, errdefs.Validationf(
				"delegation service resource: dep %q is %T, want *delegation.LocalDirectory",
				DirectoryDep, value)
		}
		directory = dir
	}
	if directory == nil {
		return nil, errdefs.Validationf(
			"delegation service resource: dep %q is required", DirectoryDep)
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

	if value, ok := in.Dep(SessionProviderDep); ok {
		provider, ok := value.(SessionProvider)
		if !ok || isNilInterface(provider) {
			return nil, errdefs.Validationf(
				"delegation service resource: dep %q is %T, want delegation.SessionProvider",
				SessionProviderDep, value)
		}
		options = append(options, WithSessionProvider(provider))
	}

	return NewService(directory, backend, options...)
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

// directoryFactory builds a per-generation LocalDirectory that binds
// itself to the assembled deployment during the deploy wire phase.
type directoryFactory struct{}

// NewDirectoryFactory returns a deployment factory for the local
// delegation directory resource. Each deployment generation builds and
// binds its own directory, so reloads always delegate against the current
// generation's agents.
func NewDirectoryFactory() res.Factory {
	return directoryFactory{}
}

// Spec implements res.Factory.
func (directoryFactory) Spec() res.Spec {
	return res.Spec{
		Kind: DirectoryKind,
		Impl: "local",
	}
}

// New implements res.Factory: a directory takes no settings and starts
// unbound; the deploy wire phase binds it to its generation's result.
func (directoryFactory) New(_ context.Context, in res.Input) (any, error) {
	if _, err := res.DecodeTyped[struct{}](in.Settings); err != nil {
		return nil, errdefs.Validationf(
			"delegation directory resource: decode settings: %v", err)
	}
	return NewDirectory(), nil
}

// RegisterDirectory adds the local directory resource factory to r.
func RegisterDirectory(r *res.Registry) error {
	return r.Register(NewDirectoryFactory())
}

// randomSessionProviderFactory builds the default ephemeral identity
// policy.
type randomSessionProviderFactory struct{}

// NewRandomSessionProviderFactory returns a deployment factory for the
// default delegation session provider: every delegation mints a fresh
// ContextID and never persists.
func NewRandomSessionProviderFactory() res.Factory {
	return randomSessionProviderFactory{}
}

// Spec implements res.Factory.
func (randomSessionProviderFactory) Spec() res.Spec {
	return res.Spec{
		Kind: SessionProviderKind,
		Impl: "random",
	}
}

// New implements res.Factory.
func (randomSessionProviderFactory) New(_ context.Context, in res.Input) (any, error) {
	if _, err := res.DecodeTyped[struct{}](in.Settings); err != nil {
		return nil, errdefs.Validationf(
			"delegation session provider resource: decode settings: %v", err)
	}
	return RandomSessionProvider{}, nil
}

// RegisterSessionProvider adds the random session provider factory to r.
func RegisterSessionProvider(r *res.Registry) error {
	return r.Register(NewRandomSessionProviderFactory())
}
