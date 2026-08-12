package runtime

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// HostFactoryDecorator wraps the runtime's built-in base host factory.
// The decorator receives the base factory (event publishing, interrupts,
// ask-user, checkpointing) and MUST delegate anything it does not
// override back to it — the canonical shape is
// agent.HostFuncs{Inner: base, ...}.
type HostFactoryDecorator func(session.HostFactory) (session.HostFactory, error)

// Builder transactionally assembles one Runtime over a resource
// registry. It is single-use.
type Builder struct {
	reg *resource.Registry

	mu            sync.Mutex
	used          bool
	hostDecorator HostFactoryDecorator
}

// NewBuilder creates a Runtime builder over a resource registry. The
// registry must already hold every resource factory the document
// references (event bus, checkpoint store, tool assemblies, engines,
// hooks, ...).
func NewBuilder(reg *resource.Registry) *Builder {
	return &Builder{reg: reg}
}

// WithHostFactory installs a decorator over the runtime's base host
// factory. It is rejected when nil or after Build starts.
func (b *Builder) WithHostFactory(decorator HostFactoryDecorator) error {
	if b == nil {
		return errdefs.Validationf("runtime Builder is nil")
	}
	if decorator == nil {
		return errdefs.Validationf("runtime host factory decorator is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return ErrBuilderUsed
	}
	if b.hostDecorator != nil {
		return errdefs.Validationf("runtime host factory decorator is already set")
	}
	b.hostDecorator = decorator
	return nil
}

// Build constructs one fully started Runtime. A Builder may be used for
// exactly one Build attempt.
func (b *Builder) Build(ctx context.Context, doc deploy.Document) (*Runtime, error) {
	if b == nil {
		return nil, errdefs.Validationf("runtime Builder is nil")
	}
	b.mu.Lock()
	if b.used {
		b.mu.Unlock()
		return nil, ErrBuilderUsed
	}
	b.used = true
	reg := b.reg
	b.mu.Unlock()

	if isNilContext(ctx) {
		return nil, errdefs.Validationf("runtime Build context is required")
	}
	if reg == nil {
		return nil, errdefs.Validationf("runtime resource registry is required")
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("runtime validate deployment: %w", err)
	}
	cfg, err := DecodeConfig(doc)
	if err != nil {
		return nil, err
	}

	result, err := deploy.NewBuilder(reg).Deploy(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("runtime build deployment: %w", err)
	}

	bus, err := resolveValue[event.Bus](result, cfg.EventBus, "event_bus")
	if err != nil {
		_ = result.Close()
		return nil, err
	}
	var checkpoints agent.CheckpointStore
	if cfg.CheckpointStore != "" {
		checkpoints, err = resolveValue[agent.CheckpointStore](
			result, cfg.CheckpointStore, "checkpoint_store")
		if err != nil {
			_ = result.Close()
			return nil, err
		}
	}

	baseHostFactory, err := newBaseHostFactory(bus, checkpoints)
	if err != nil {
		_ = result.Close()
		return nil, err
	}
	hostFactory := baseHostFactory
	if b.hostDecorator != nil {
		hostFactory, err = b.hostDecorator(baseHostFactory)
		if err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("runtime decorate base host factory: %w", err)
		}
		if isNil(hostFactory) {
			_ = result.Close()
			return nil, errdefs.Internalf(
				"runtime host factory decorator returned nil")
		}
	}
	hostFactory, err = wrapDelegation(hostFactory, result)
	if err != nil {
		_ = result.Close()
		return nil, err
	}

	var catalogProvider session.CatalogProvider
	if cfg.DynamicCatalog != nil {
		assemblies, resolveErr := resolveDynamicCatalogAssemblies(
			doc, result, cfg.DynamicCatalog)
		if resolveErr != nil {
			_ = result.Close()
			return nil, resolveErr
		}
		catalogProvider = newDynamicCatalogProvider(assemblies)
	}

	// The session coordinator must observe every run event and stream
	// delta in order; a dropping subscription could lose run-end
	// delimiters or confirmed deltas, so the router blocks instead of
	// using the bus default.
	router := event.NewRouter(bus, event.WithDefaultAttachBackpressure(event.Block))
	managerOptions := []session.ManagerOption{
		session.WithIdleTimeout(cfg.Sessions.IdleTimeout),
		session.WithSinkBufferSize(cfg.Sessions.SinkBuffer),
		session.WithSpeculativeBufferLimits(
			cfg.Sessions.SpeculativeBufferEvents,
			cfg.Sessions.SpeculativeBufferBytes,
		),
	}
	if checkpoints != nil {
		managerOptions = append(managerOptions,
			session.WithCheckpointStore(checkpoints),
			session.WithResume(cfg.Sessions.Resume),
		)
	}
	if catalogProvider != nil {
		managerOptions = append(managerOptions,
			session.WithCatalogProvider(catalogProvider))
	}
	manager, err := session.NewManager(
		resultResolver{result: result}, hostFactory, router, managerOptions...)
	if err != nil {
		_ = router.Close()
		_ = result.Close()
		return nil, fmt.Errorf("runtime create session manager: %w", err)
	}
	return &Runtime{manager: manager, router: router, result: result}, nil
}

// resultResolver adapts core deploy.Result to the session manager's
// InstanceResolver (method name differs: Agent vs Instance).
type resultResolver struct {
	result *deploy.Result
}

func (r resultResolver) Instance(id string) (*agent.Agent, bool) {
	return r.result.Agent(id)
}

func resolveValue[T any](result *deploy.Result, name, field string) (T, error) {
	var zero T
	value, ok := result.Value(name)
	if !ok {
		return zero, errdefs.NotFoundf(
			"runtime config: %s resource %q not found", field, name)
	}
	typed, ok := value.(T)
	if !ok || isNil(typed) {
		return zero, errdefs.Validationf(
			"runtime config: %s resource %q is %T, want %v",
			field, name, value, reflect.TypeFor[T]())
	}
	return typed, nil
}

// wrapDelegation exposes a delegation.Service found among the built
// resources on every turn host via delegation.WithService.
func wrapDelegation(
	hostFactory session.HostFactory,
	result *deploy.Result,
) (session.HostFactory, error) {
	var service delegation.Service
	for _, name := range result.Names() {
		value, _ := result.Value(name)
		if candidate, ok := value.(delegation.Service); ok && !isNil(candidate) {
			if !isNil(service) {
				return nil, errdefs.Conflictf(
					"runtime: multiple delegation services built (%s)", name)
			}
			service = candidate
		}
	}
	if isNil(service) {
		return hostFactory, nil
	}
	return delegationHostFactory{inner: hostFactory, service: service}, nil
}

type delegationHostFactory struct {
	inner   session.HostFactory
	service delegation.Service
}

func (f delegationHostFactory) NewHost(
	ctx context.Context,
	request session.HostRequest,
) (agent.Host, error) {
	host, err := f.inner.NewHost(ctx, request)
	if err != nil {
		return nil, err
	}
	return delegation.WithService(host, f.service), nil
}

func isNilContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v := reflect.ValueOf(ctx)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
