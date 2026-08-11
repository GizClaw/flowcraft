package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

const (
	eventBusResourceKind        = "event.Bus"
	checkpointStoreResourceKind = "agent.CheckpointStore"
)

type registeredIntegration struct {
	spec    IntegrationSpec
	factory IntegrationFactory
}

// HostFactoryDecorator wraps the runtime's built-in base host factory.
// The decorator receives the base factory (event publishing, interrupts,
// ask-user, checkpointing, usage reporting) and MUST delegate anything it
// does not override back to it — the canonical shape is
// agent.HostFuncs{Inner: base, ...} — so a decorated host can only
// enhance, never drop, core session behaviour.
type HostFactoryDecorator func(session.HostFactory) (session.HostFactory, error)

// Builder transactionally assembles one Runtime. It is single-use.
type Builder struct {
	deploy *deploy.Builder

	mu            sync.Mutex
	used          bool
	factories     map[string]registeredIntegration
	hostDecorator HostFactoryDecorator
}

// NewBuilder creates a Runtime builder over a deployment builder.
func NewBuilder(deployBuilder *deploy.Builder) *Builder {
	return &Builder{
		deploy:    deployBuilder,
		factories: make(map[string]registeredIntegration),
	}
}

// WithHostFactory installs a decorator over the runtime's base host
// factory. The decorator runs after the base factory is constructed and
// before runtime integrations decorate the chain, so integrations always
// wrap the decorated factory. It is rejected when nil or after Build
// starts. Without it the runtime uses the built-in base factory unchanged.
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

// RegisterIntegration registers one factory kind. Registration is rejected
// after Build starts.
func (b *Builder) RegisterIntegration(factory IntegrationFactory) error {
	if b == nil {
		return errdefs.Validationf("runtime Builder is nil")
	}
	if isNil(factory) {
		return errdefs.Validationf("runtime integration factory is nil")
	}
	spec := cloneIntegrationSpec(factory.Spec())
	if err := validateIntegrationSpec(spec); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return ErrBuilderUsed
	}
	if _, exists := b.factories[spec.Kind]; exists {
		return errdefs.Validationf(
			"runtime integration factory kind %q is already registered", spec.Kind)
	}
	b.factories[spec.Kind] = registeredIntegration{spec: spec, factory: factory}
	return nil
}

// Build constructs one fully started Runtime or rolls back every acquired
// object. A Builder may be used for exactly one Build attempt.
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
	catalog := make(map[string]registeredIntegration, len(b.factories))
	for kind, registered := range b.factories {
		catalog[kind] = registered
	}
	deployBuilder := b.deploy
	b.mu.Unlock()

	if isNil(ctx) {
		return nil, errdefs.Validationf("runtime Build context is required")
	}
	if deployBuilder == nil {
		return nil, errdefs.Validationf("runtime deploy Builder is required")
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("runtime validate deployment: %w", err)
	}
	cfg, err := DecodeConfig(doc)
	if err != nil {
		return nil, err
	}
	resolved, externalNames, err := resolveCatalog(doc, cfg, catalog)
	if err != nil {
		return nil, err
	}

	owned := ownedResources{
		integrations:  make([]preparedRecord, 0, len(cfg.Integrations)),
		schedulerName: cfg.Scheduler,
	}
	fail := func(primary error) error {
		return errors.Join(primary, owned.rollback(ctx), owned.close())
	}

	for i, item := range cfg.Integrations {
		integration, prepareErr := resolved[i].factory.Prepare(ctx, PrepareInput{
			Name: item.Name, Kind: item.Kind, Settings: item.Settings,
			Deploy: deployBuilder,
		})
		if !isNil(integration) {
			owned.integrations = append(owned.integrations, preparedRecord{
				name: item.Name, integration: integration,
			})
		}
		if prepareErr != nil {
			return nil, fail(fmt.Errorf(
				"runtime integration %q prepare: %w", item.Name, prepareErr))
		}
		if isNil(integration) {
			return nil, fail(errdefs.Internalf(
				"runtime integration %q prepare returned nil", item.Name))
		}
	}

	result, err := deployBuilder.Build(
		ctx, doc, deploy.WithExternalResourceConsumers(externalNames...))
	if err != nil {
		return nil, fail(fmt.Errorf("runtime build deployment: %w", err))
	}
	owned.result = result

	var catalogProvider session.CatalogProvider
	if cfg.Sessions.DynamicCatalog != nil {
		assemblies, err := resolveDynamicCatalogAssemblies(
			doc, result, cfg.Sessions.DynamicCatalog)
		if err != nil {
			return nil, fail(err)
		}
		catalogProvider = newDynamicCatalogProvider(
			assemblies, cfg.Sessions.DynamicCatalog)
	}

	schedulerServer, err := resolveScheduler(result, cfg.Scheduler)
	if err != nil {
		return nil, fail(err)
	}
	owned.scheduler = schedulerServer
	bus, err := resolveEventBus(doc, result, cfg.EventBus)
	if err != nil {
		return nil, fail(err)
	}
	checkpoints, err := resolveCheckpointStore(doc, result, cfg.CheckpointStore)
	if err != nil {
		return nil, fail(err)
	}
	baseHostFactory, err := newBaseHostFactory(bus, checkpoints)
	if err != nil {
		return nil, fail(err)
	}
	hostFactory := baseHostFactory
	if b.hostDecorator != nil {
		hostFactory, err = b.hostDecorator(baseHostFactory)
		if err != nil {
			return nil, fail(fmt.Errorf("runtime decorate base host factory: %w", err))
		}
		if isNil(hostFactory) {
			return nil, fail(errdefs.Internalf(
				"runtime host factory decorator returned nil"))
		}
	}
	for i, item := range cfg.Integrations {
		dependencies, depErr := resolveDependencies(
			doc, result, item, resolved[i].spec, cfg.Scheduler)
		if depErr != nil {
			return nil, fail(depErr)
		}
		if bindErr := owned.integrations[i].integration.Bind(ctx, BindInput{
			Name:         item.Name,
			Dependencies: dependencies,
			Deployment:   result,
			BaseHosts:    baseHostFactory,
		}); bindErr != nil {
			return nil, fail(fmt.Errorf(
				"runtime integration %q bind: %w", item.Name, bindErr))
		}
	}

	// Wrapping in reverse configuration order makes integrations[0] the
	// outermost decorator while preserving each decorator's ordinary semantics.
	for _, v := range slices.Backward(owned.integrations) {
		if isNil(hostFactory) {
			return nil, fail(errdefs.Internalf(
				"runtime integration %q received a nil HostFactory", v.name))
		}
		hostFactory, err = v.integration.DecorateHost(hostFactory)
		if err != nil {
			return nil, fail(fmt.Errorf(
				"runtime integration %q decorate host: %w", v.name, err))
		}
		if isNil(hostFactory) {
			return nil, fail(errdefs.Internalf(
				"runtime integration %q decorate host returned nil", v.name))
		}
	}

	router := agent.NewStreamRouter(
		bus,
		agent.WithStreamIncludeAllRunEvents(),
		agent.WithStreamSubOptions(event.WithBackpressure(event.Block)),
	)
	owned.router = router
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
	manager, err := session.NewManager(result, hostFactory, router, managerOptions...)
	if err != nil {
		return nil, fail(fmt.Errorf("runtime create session manager: %w", err))
	}
	owned.manager = manager
	for i, record := range owned.integrations {
		if startErr := record.integration.Start(ctx); startErr != nil {
			return nil, fail(fmt.Errorf(
				"runtime integration %q start: %w", cfg.Integrations[i].Name, startErr))
		}
	}
	if starter, ok := schedulerServer.(interface{ Start() error }); ok {
		if startErr := starter.Start(); startErr != nil {
			return nil, fail(fmt.Errorf("runtime start scheduler %q: %w", cfg.Scheduler, startErr))
		}
	}

	return &Runtime{
		manager: manager, scheduler: schedulerServer, schedulerName: cfg.Scheduler, router: router,
		integrations: owned.integrations, result: result,
	}, nil
}

func resolveCatalog(
	doc deploy.Document,
	cfg Config,
	catalog map[string]registeredIntegration,
) ([]registeredIntegration, []string, error) {
	eventEntry, ok := doc.Resources[cfg.EventBus]
	if !ok {
		return nil, nil, errdefs.NotFoundf(
			"runtime config event_bus %q: resource is not defined", cfg.EventBus)
	}
	if eventEntry.Kind != eventBusResourceKind {
		return nil, nil, errdefs.Validationf(
			"runtime config event_bus %q has kind %q, want %q",
			cfg.EventBus, eventEntry.Kind, eventBusResourceKind)
	}
	external := []string{cfg.EventBus}
	if cfg.CheckpointStore != "" {
		checkpointEntry, exists := doc.Resources[cfg.CheckpointStore]
		if !exists {
			return nil, nil, errdefs.NotFoundf(
				"runtime config checkpoint_store %q: resource is not defined",
				cfg.CheckpointStore)
		}
		if checkpointEntry.Kind != checkpointStoreResourceKind {
			return nil, nil, errdefs.Validationf(
				"runtime config checkpoint_store %q has kind %q, want %q",
				cfg.CheckpointStore, checkpointEntry.Kind, checkpointStoreResourceKind)
		}
		external = append(external, cfg.CheckpointStore)
	}

	resolved := make([]registeredIntegration, len(cfg.Integrations))
	if cfg.Scheduler != "" {
		schedulerEntry, exists := doc.Resources[cfg.Scheduler]
		if !exists {
			return nil, nil, errdefs.NotFoundf(
				"runtime config scheduler %q: resource is not defined", cfg.Scheduler)
		}
		if schedulerEntry.Kind != schedulerconfig.ResourceKind {
			return nil, nil, errdefs.Validationf(
				"runtime config scheduler %q has kind %q, want %q",
				cfg.Scheduler, schedulerEntry.Kind, schedulerconfig.ResourceKind)
		}
		resourceNames := make([]string, 0, len(doc.Resources))
		for resourceName := range doc.Resources {
			resourceNames = append(resourceNames, resourceName)
		}
		slices.Sort(resourceNames)
		for _, resourceName := range resourceNames {
			if resourceName == cfg.Scheduler {
				continue
			}
			entry := doc.Resources[resourceName]
			depNames := make([]string, 0, len(entry.Deps))
			for depName := range entry.Deps {
				depNames = append(depNames, depName)
			}
			slices.Sort(depNames)
			for _, depName := range depNames {
				if entry.Deps[depName].Resource == cfg.Scheduler {
					return nil, nil, errdefs.Validationf(
						"runtime scheduler %q cannot be dependency %q of deployment resource %q",
						cfg.Scheduler, depName, resourceName)
				}
			}
		}
		external = append(external, cfg.Scheduler)
	}
	if cfg.Sessions.DynamicCatalog != nil {
		for agentID, resourceName := range cfg.Sessions.DynamicCatalog.Tools {
			entry, exists := doc.Resources[resourceName]
			if !exists {
				return nil, nil, errdefs.NotFoundf(
					"runtime config sessions.dynamic_catalog.tools[%q]: resource %q is not defined",
					agentID, resourceName)
			}
			if entry.Kind != toolconfig.ResourceKind {
				return nil, nil, errdefs.Validationf(
					"runtime config sessions.dynamic_catalog.tools[%q]: resource %q has kind %q, want %q",
					agentID, resourceName, entry.Kind, toolconfig.ResourceKind)
			}
			external = append(external, resourceName)
		}
	}
	for i, item := range cfg.Integrations {
		registered, exists := catalog[item.Kind]
		if !exists {
			return nil, nil, errdefs.NotFoundf(
				"runtime integration %q: kind %q is not registered",
				item.Name, item.Kind)
		}
		declared := make(map[string]DependencySpec, len(registered.spec.Deps))
		for _, spec := range registered.spec.Deps {
			declared[spec.Name] = spec
		}
		for depName, resourceName := range item.Deps {
			if _, exists := declared[depName]; !exists {
				return nil, nil, errdefs.Validationf(
					"runtime integration %q: dependency %q is undeclared",
					item.Name, depName)
			}
			if declared[depName].Kind == schedulerconfig.ResourceKind {
				return nil, nil, errdefs.Validationf(
					"runtime integration %q dependency %q must be configured through runtime.scheduler",
					item.Name, depName)
			}
			if _, exists := doc.Resources[resourceName]; !exists {
				return nil, nil, errdefs.NotFoundf(
					"runtime integration %q dependency %q: resource %q is not defined",
					item.Name, depName, resourceName)
			}
			external = append(external, resourceName)
		}
		for _, spec := range registered.spec.Deps {
			if spec.Kind == schedulerconfig.ResourceKind {
				if spec.Required && cfg.Scheduler == "" {
					return nil, nil, errdefs.Validationf(
						"runtime integration %q dependency %q requires runtime.scheduler",
						item.Name, spec.Name)
				}
				continue
			}
			if spec.Required {
				if _, exists := item.Deps[spec.Name]; !exists {
					return nil, nil, errdefs.NotFoundf(
						"runtime integration %q: required dependency %q is not configured",
						item.Name, spec.Name)
				}
			}
		}
		resolved[i] = registered
	}
	slices.Sort(external)
	external = slices.Compact(external)
	return resolved, external, nil
}

func resolveEventBus(
	doc deploy.Document,
	result *deploy.Result,
	name string,
) (event.Bus, error) {
	entry, ok := doc.Resources[name]
	if !ok {
		return nil, errdefs.NotFoundf(
			"runtime event bus resource %q is not defined", name)
	}
	if entry.Kind != eventBusResourceKind {
		return nil, errdefs.Validationf(
			"runtime event bus resource %q has kind %q, want %q",
			name, entry.Kind, eventBusResourceKind)
	}
	value, err := result.Resource(name)
	if err != nil {
		return nil, fmt.Errorf("runtime resolve event bus %q: %w", name, err)
	}
	bus, ok := value.(event.Bus)
	if !ok {
		return nil, errdefs.Validationf(
			"runtime event bus resource %q has Go type %T, want %v",
			name, value, reflect.TypeFor[event.Bus]())
	}
	if isNil(bus) {
		return nil, errdefs.Internalf(
			"runtime event bus resource %q is nil", name)
	}
	return bus, nil
}

func resolveCheckpointStore(
	doc deploy.Document,
	result *deploy.Result,
	name string,
) (agent.CheckpointStore, error) {
	if name == "" {
		return nil, nil
	}
	entry, ok := doc.Resources[name]
	if !ok {
		return nil, errdefs.NotFoundf(
			"runtime checkpoint store resource %q is not defined", name)
	}
	if entry.Kind != checkpointStoreResourceKind {
		return nil, errdefs.Validationf(
			"runtime checkpoint store resource %q has kind %q, want %q",
			name, entry.Kind, checkpointStoreResourceKind)
	}
	value, err := result.Resource(name)
	if err != nil {
		return nil, fmt.Errorf("runtime resolve checkpoint store %q: %w", name, err)
	}
	store, ok := value.(agent.CheckpointStore)
	if !ok {
		return nil, errdefs.Validationf(
			"runtime checkpoint store resource %q has Go type %T, want %v",
			name, value, reflect.TypeFor[agent.CheckpointStore]())
	}
	if isNil(store) {
		return nil, errdefs.Internalf(
			"runtime checkpoint store resource %q is nil", name)
	}
	return store, nil
}

func resolveScheduler(
	result *deploy.Result,
	name string,
) (sdkscheduler.Server, error) {
	if name == "" {
		return nil, nil
	}
	value, err := result.Resource(name)
	if err != nil {
		return nil, fmt.Errorf("runtime resolve scheduler %q: %w", name, err)
	}
	server, ok := value.(sdkscheduler.Server)
	if !ok {
		return nil, errdefs.Validationf(
			"runtime scheduler resource %q has Go type %T, want %v",
			name, value, reflect.TypeFor[sdkscheduler.Server]())
	}
	if isNil(server) {
		return nil, errdefs.Internalf(
			"runtime scheduler resource %q is nil", name)
	}
	return server, nil
}

func resolveDependencies(
	doc deploy.Document,
	result *deploy.Result,
	cfg IntegrationConfig,
	spec IntegrationSpec,
	schedulerName string,
) (Dependencies, error) {
	declared := make(map[string]DependencySpec, len(spec.Deps))
	for _, dep := range spec.Deps {
		declared[dep.Name] = dep
	}
	bindings := make(map[string]string, len(cfg.Deps)+1)
	for depName, resourceName := range cfg.Deps {
		bindings[depName] = resourceName
	}
	for _, dep := range spec.Deps {
		if dep.Kind == schedulerconfig.ResourceKind && schedulerName != "" {
			if _, exists := bindings[dep.Name]; !exists {
				bindings[dep.Name] = schedulerName
			}
		}
	}
	values := make(map[string]any, len(bindings))
	for depName, resourceName := range bindings {
		dep := declared[depName]
		entry, exists := doc.Resources[resourceName]
		if !exists {
			return Dependencies{}, errdefs.NotFoundf(
				"runtime integration %q dependency %q: resource %q is not defined",
				cfg.Name, depName, resourceName)
		}
		if entry.Kind != dep.Kind {
			return Dependencies{}, errdefs.Validationf(
				"runtime integration %q dependency %q: resource %q has kind %q, want %q",
				cfg.Name, depName, resourceName, entry.Kind, dep.Kind)
		}
		value, err := result.Resource(resourceName)
		if err != nil {
			return Dependencies{}, fmt.Errorf(
				"runtime integration %q dependency %q: %w", cfg.Name, depName, err)
		}
		actual := reflect.TypeOf(value)
		if actual == nil || !actual.AssignableTo(dep.Type) {
			return Dependencies{}, errdefs.Validationf(
				"runtime integration %q dependency %q: resource %q has Go type %T, want %v",
				cfg.Name, depName, resourceName, value, dep.Type)
		}
		if isNil(value) {
			return Dependencies{}, errdefs.Internalf(
				"runtime integration %q dependency %q: resource %q is nil",
				cfg.Name, depName, resourceName)
		}
		values[depName] = value
	}
	return Dependencies{values: values}, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
