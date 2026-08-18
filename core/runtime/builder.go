package runtime

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
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

// ResultHostFactoryDecorator wraps the runtime's host factory with access to
// the fully assembled deployment. It runs after any HostFactoryDecorator, so
// the deployment-aware layer sits outermost and can expose deployment-built
// services (e.g. delegation) on every turn host. The result is borrowed only
// for the duration of the call.
type ResultHostFactoryDecorator func(result *deploy.Result, factory session.HostFactory) (session.HostFactory, error)

// Builder transactionally assembles one Runtime over a resource
// registry. It is single-use.
type Builder struct {
	reg *resource.Registry

	mu                  sync.Mutex
	used                bool
	hostDecorator       HostFactoryDecorator
	resultHostDecorator ResultHostFactoryDecorator
	loader              *resource.Loader
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

// WithResultHostFactory installs a decorator that runs after WithHostFactory
// with access to the fully assembled deployment. It is rejected when nil or
// after Build starts.
func (b *Builder) WithResultHostFactory(decorator ResultHostFactoryDecorator) error {
	if b == nil {
		return errdefs.Validationf("runtime Builder is nil")
	}
	if decorator == nil {
		return errdefs.Validationf("runtime result host factory decorator is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return ErrBuilderUsed
	}
	if b.resultHostDecorator != nil {
		return errdefs.Validationf("runtime result host factory decorator is already set")
	}
	b.resultHostDecorator = decorator
	return nil
}

// WithLoader installs the deployment-level loader used to materialize
// {"file":…} / {"embed":…} settings subtrees and agent engine/hook
// settings. It is rejected after Build starts.
func (b *Builder) WithLoader(loader *resource.Loader) error {
	if b == nil {
		return errdefs.Validationf("runtime Builder is nil")
	}
	if loader == nil {
		return errdefs.Validationf("runtime loader is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return ErrBuilderUsed
	}
	if b.loader != nil {
		return errdefs.Validationf("runtime loader is already set")
	}
	b.loader = loader
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

	deployBuilder := deploy.NewBuilder(reg)
	if b.loader != nil {
		deployBuilder = deploy.NewBuilder(reg, deploy.WithLoader(b.loader))
	}
	result, err := deployBuilder.Deploy(ctx, doc)
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
	if b.resultHostDecorator != nil {
		hostFactory, err = b.resultHostDecorator(result, hostFactory)
		if err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("runtime decorate host factory with deployment: %w", err)
		}
		if isNil(hostFactory) {
			_ = result.Close()
			return nil, errdefs.Internalf(
				"runtime result host factory decorator returned nil")
		}
	}

	var catalogProvider session.CatalogProvider
	var liveCatalog *catalogRegistry
	if cfg.DynamicCatalog != nil {
		assemblies, resolveErr := resolveDynamicCatalogAssemblies(
			doc, result, cfg.DynamicCatalog)
		if resolveErr != nil {
			_ = result.Close()
			return nil, resolveErr
		}
		liveCatalog = newCatalogRegistry(assemblies)
		catalogProvider = liveCatalog
	}

	router := event.NewRouter(bus)
	registry := newAgentRegistry(result)
	initial := &Generation{
		id:          1,
		doc:         doc,
		registry:    registry,
		result:      result,
		bus:         bus,
		hostFactory: hostFactory,
		resolver:    generationResolver{registry: registry, result: result},
		catalog:     liveCatalog,
	}
	managerOptions := []session.ManagerOption{
		session.WithIdleTimeout(cfg.Sessions.IdleTimeout),
		session.WithSinkBufferSize(cfg.Sessions.SinkBuffer),
		session.WithDeliveryConcurrency(cfg.Sessions.DeliveryConcurrency),
		session.WithMaxSessions(cfg.Sessions.MaxSessions),
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
	manager, err := session.NewManager(initial.resolver, hostFactory, router, managerOptions...)
	if err != nil {
		_ = router.Close()
		_ = result.Close()
		return nil, fmt.Errorf("runtime create session manager: %w", err)
	}
	return &Runtime{
		manager:             manager,
		router:              router,
		result:              result,
		registry:            registry,
		liveCatalog:         liveCatalog,
		resources:           reg,
		loader:              b.loader,
		bus:                 bus,
		hostDecorator:       b.hostDecorator,
		resultHostDecorator: b.resultHostDecorator,
		current:             initial,
		nextGenID:           1,
	}, nil
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
