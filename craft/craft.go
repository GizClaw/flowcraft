package craft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/ui"
)

// Craft is one running application: shared services plus a keyed set of
// runtimes. The zero value is not usable; construct with New.
type Craft struct {
	def  Definition
	opts Options
	caps []Capability

	registry         *resource.Registry
	definitionLayers []deploy.Layer
	bus              *event.MemoryBus
	router           *event.Router
	plugins          PluginHost
	uiRegistry       *ui.Registry
	hostMCP          *hostMCPState
	watchCancel      context.CancelFunc
	unsubscribe      func()

	// mu serializes lifecycle mutations: Start, OpenRuntime,
	// ReloadRuntime, CloseRuntime and Close. Binder callbacks run after
	// the mutation and outside the lock.
	mu          sync.Mutex
	started     bool
	closed      bool
	opening     map[RuntimeKey]struct{}
	runtimes    map[RuntimeKey]*runtime.Runtime
	runtimeOpts map[RuntimeKey]RuntimeOptions
}

// New validates the definition and builds the shared services. No
// runtime is created until OpenRuntime.
func New(def Definition, opts Options) (*Craft, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}
	if opts.AppHome == "" {
		opts.AppHome = opts.DataDir
	}
	if opts.Loader == nil {
		loaderOpts := []resource.LoaderOption{
			resource.WithBaseDir(opts.DefinitionDir),
		}
		if opts.Assets != nil {
			loaderOpts = append(loaderOpts, resource.WithEmbed(opts.Assets))
		}
		opts.Loader = resource.NewLoader(loaderOpts...)
	}

	registry := resource.NewRegistry()
	caps := make([]Capability, 0, len(opts.Capabilities))
	seen := make(map[string]struct{}, len(opts.Capabilities))
	for i, capability := range opts.Capabilities {
		if capability == nil {
			return nil, errdefs.Validationf("craft: capability[%d] is nil", i)
		}
		name := strings.TrimSpace(capability.Name())
		if name == "" {
			return nil, errdefs.Validationf(
				"craft: capability[%d] has an empty name", i)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, errdefs.Conflictf("craft: duplicate capability %q", name)
		}
		seen[name] = struct{}{}
		if registrar, ok := capability.(Registrar); ok {
			if err := registrar.Register(registry); err != nil {
				return nil, fmt.Errorf(
					"craft: capability %s register: %w", name, err)
			}
		}
		caps = append(caps, capability)
	}
	if opts.Plugins != nil {
		if err := registry.Register(pluginNodeFactory{host: opts.Plugins}); err != nil {
			return nil, fmt.Errorf("craft: register plugin node type: %w", err)
		}
	}

	bus := event.NewMemoryBus()
	c := &Craft{
		def:         def,
		opts:        opts,
		caps:        caps,
		registry:    registry,
		bus:         bus,
		router:      event.NewRouter(bus),
		plugins:     opts.Plugins,
		opening:     make(map[RuntimeKey]struct{}),
		runtimes:    make(map[RuntimeKey]*runtime.Runtime),
		runtimeOpts: make(map[RuntimeKey]RuntimeOptions),
	}
	if c.plugins != nil {
		uiRegistry, err := ui.NewRegistry(c.plugins, c)
		if err != nil {
			_ = c.router.Close()
			_ = c.bus.Close()
			return nil, fmt.Errorf("craft: ui registry: %w", err)
		}
		c.uiRegistry = uiRegistry
	}
	named := hostmcp.NewServiceRegistry()
	for _, capability := range caps {
		registrar, ok := capability.(ServiceRegistrar)
		if !ok {
			continue
		}
		if err := registrar.RegisterServices(named); err != nil {
			_ = c.router.Close()
			_ = c.bus.Close()
			return nil, fmt.Errorf(
				"craft: capability %s services: %w", capability.Name(), err)
		}
	}
	services := hostServiceSet(caps)
	direct := opts.HostServices
	if direct.Secrets != nil {
		services.Secrets = direct.Secrets
	}
	if direct.Context != nil {
		services.Context = direct.Context
	}
	if direct.Browser != nil {
		services.Browser = direct.Browser
	}
	if direct.Inference != nil {
		services.Inference = direct.Inference
	}
	if direct.Sessions != nil {
		services.Sessions = direct.Sessions
	}
	if direct.Telemetry != nil {
		services.Telemetry = direct.Telemetry
	}
	if direct.Events != nil {
		services.Events = direct.Events
	}
	var disabled []string
	if def.HostTools != nil {
		disabled = append(disabled, def.HostTools.Disable...)
		if err := applyServiceBindings(
			def.HostTools.Services, named, &services); err != nil {
			_ = c.router.Close()
			_ = c.bus.Close()
			return nil, err
		}
	}
	if services.Secrets != nil || services.Context != nil ||
		services.Browser != nil || services.Inference != nil ||
		services.Sessions != nil || services.Telemetry != nil ||
		services.Events != nil {
		hostMCP, err := newHostMCP(services, disabled, Version)
		if err != nil {
			_ = c.router.Close()
			_ = c.bus.Close()
			return nil, err
		}
		c.hostMCP = hostMCP
		c.installPluginHooks()
	}
	layers, err := c.composeDefinitionLayers()
	if err != nil {
		_ = c.router.Close()
		_ = c.bus.Close()
		return nil, err
	}
	c.definitionLayers = layers
	return c, nil
}

// Start starts the shared services. It is idempotent.
func (c *Craft) Start(ctx context.Context) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: Start context is required")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrCraftClosed
	}
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.started = true
	c.mu.Unlock()
	if err := c.startHostMCP(); err != nil {
		return err
	}
	if c.plugins != nil {
		if err := c.plugins.Start(ctx); err != nil {
			return err
		}
		c.startPluginWatch()
	}
	c.publish(ctx, SubjectCraftStarted, RuntimeEvent{})
	return nil
}

// OpenRuntime composes the layers for key, builds a runtime and binds
// runtime services. A duplicate key is a conflict.
func (c *Craft) OpenRuntime(
	ctx context.Context,
	key RuntimeKey,
	opts RuntimeOptions,
) (*runtime.Runtime, error) {
	if ptr.IsNil(ctx) {
		return nil, errdefs.Validationf("craft: OpenRuntime context is required")
	}
	if strings.TrimSpace(string(key)) == "" {
		return nil, errdefs.Validationf("craft: runtime key is required")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrCraftClosed
	}
	if !c.started {
		c.mu.Unlock()
		return nil, ErrNotStarted
	}
	if _, exists := c.runtimes[key]; exists {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrRuntimeExists, key)
	}
	if _, opening := c.opening[key]; opening {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is opening", ErrRuntimeExists, key)
	}
	c.opening[key] = struct{}{}
	c.mu.Unlock()

	fail := func(err error) (*runtime.Runtime, error) {
		c.mu.Lock()
		delete(c.opening, key)
		c.mu.Unlock()
		return nil, err
	}
	doc, externals, resolver, err := c.compose(ctx, key, opts)
	if err != nil {
		return fail(err)
	}
	rt, err := c.buildRuntime(ctx, doc, externals, resolver, opts)
	if err != nil {
		return fail(err)
	}
	c.mu.Lock()
	delete(c.opening, key)
	c.runtimes[key] = rt
	c.runtimeOpts[key] = opts
	c.mu.Unlock()

	if err := c.bindRuntime(ctx, key, rt); err != nil {
		_ = rt.Close()
		c.mu.Lock()
		delete(c.runtimes, key)
		delete(c.runtimeOpts, key)
		c.mu.Unlock()
		return nil, err
	}
	c.publish(ctx, SubjectRuntimeOpened, RuntimeEvent{Key: key})
	return rt, nil
}

// ReloadRuntime recomposes the layers for key and atomically replaces
// the runtime's deployment generation. In-flight turns stay on the
// previous generation.
func (c *Craft) ReloadRuntime(
	ctx context.Context,
	key RuntimeKey,
	reason Reason,
) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: ReloadRuntime context is required")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrCraftClosed
	}
	rt, ok := c.runtimes[key]
	opts := c.runtimeOpts[key]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	c.publish(ctx, SubjectReloadStarted, RuntimeEvent{Key: key, Reason: reason})
	doc, _, _, err := c.compose(ctx, key, opts)
	if err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return err
	}
	if _, err := rt.Reload(ctx, doc); err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return fmt.Errorf("craft: reload runtime %q: %w", key, err)
	}
	if err := c.bindRuntime(ctx, key, rt); err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return err
	}
	c.publish(ctx, SubjectReloadCompleted, RuntimeEvent{Key: key, Reason: reason})
	return nil
}

// ReloadAll reloads every open runtime. Each runtime is transactional;
// failures are aggregated with the runtime key.
func (c *Craft) ReloadAll(ctx context.Context, reason Reason) error {
	var errs []error
	for _, key := range c.Runtimes() {
		if err := c.ReloadRuntime(ctx, key, reason); err != nil {
			errs = append(errs, fmt.Errorf("runtime %q: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// DrainRuntime waits for the runtime's active turns to finish naturally.
func (c *Craft) DrainRuntime(ctx context.Context, key RuntimeKey) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: DrainRuntime context is required")
	}
	rt, ok := c.Runtime(key)
	if !ok {
		return fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	return rt.Drain(ctx)
}

// Drain waits for every runtime's active work to finish naturally. It
// is the manager-facing counterpart of DrainRuntime.
func (c *Craft) Drain(ctx context.Context) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: Drain context is required")
	}
	var errs []error
	for _, key := range c.Runtimes() {
		if err := c.DrainRuntime(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("runtime %q: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// CloseRuntime drains and closes one runtime. A drain error is returned
// joined with the close result; the runtime is closed either way.
func (c *Craft) CloseRuntime(ctx context.Context, key RuntimeKey) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: CloseRuntime context is required")
	}
	c.mu.Lock()
	rt, ok := c.runtimes[key]
	if ok {
		delete(c.runtimes, key)
		delete(c.runtimeOpts, key)
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	var errs []error
	if err := rt.Drain(ctx); err != nil {
		errs = append(errs, fmt.Errorf("craft: drain runtime %q: %w", key, err))
	}
	if err := rt.Close(); err != nil {
		errs = append(errs, fmt.Errorf("craft: close runtime %q: %w", key, err))
	}
	c.publish(ctx, SubjectRuntimeClosed, RuntimeEvent{Key: key})
	return errors.Join(errs...)
}

// RegisterAgent registers a runtime agent through the runtime's dynamic
// agent registry.
func (c *Craft) RegisterAgent(
	ctx context.Context,
	key RuntimeKey,
	name string,
	def agent.Definition,
	opts ...runtime.RegisterAgentOption,
) (*agent.Agent, error) {
	rt, ok := c.Runtime(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	return rt.RegisterAgent(ctx, name, def, opts...)
}

// UnregisterAgent removes a dynamically registered runtime agent.
func (c *Craft) UnregisterAgent(
	ctx context.Context,
	key RuntimeKey,
	name string,
	opts ...runtime.UnregisterAgentOption,
) error {
	rt, ok := c.Runtime(key)
	if !ok {
		return fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	return rt.UnregisterAgent(ctx, name, opts...)
}

// Attach subscribes to the craft-plane event router.
func (c *Craft) Attach(
	ctx context.Context,
	pattern event.Pattern,
	sink event.Sink,
	opts ...event.AttachOption,
) (func(), error) {
	if c == nil || c.router == nil {
		return nil, errdefs.Validationf("craft: Attach requires a Craft")
	}
	return c.router.Attach(ctx, pattern, sink, opts...)
}

// Emit publishes one event on the craft plane.
func (c *Craft) Emit(
	ctx context.Context,
	subject event.Subject,
	payload any,
) error {
	if c == nil || c.bus == nil {
		return errdefs.Validationf("craft: Emit requires a Craft")
	}
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: Emit context is required")
	}
	envelope, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	return c.bus.Publish(ctx, envelope)
}

// Close closes every runtime (newest key first), then the router and
// the craft bus. It is idempotent.
func (c *Craft) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.started = false
	watchCancel := c.watchCancel
	unsubscribe := c.unsubscribe
	uiRegistry := c.uiRegistry
	c.watchCancel = nil
	c.unsubscribe = nil
	c.uiRegistry = nil
	plugins := c.plugins
	keys := make([]RuntimeKey, 0, len(c.runtimes))
	for key := range c.runtimes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	runtimes := c.runtimes
	c.runtimes = make(map[RuntimeKey]*runtime.Runtime)
	c.runtimeOpts = make(map[RuntimeKey]RuntimeOptions)
	c.mu.Unlock()

	var errs []error
	if watchCancel != nil {
		watchCancel()
	}
	if unsubscribe != nil {
		unsubscribe()
	}
	if uiRegistry != nil {
		_ = uiRegistry.Close()
	}
	for i := len(keys) - 1; i >= 0; i-- {
		key := keys[i]
		if err := runtimes[key].Close(); err != nil {
			errs = append(errs, fmt.Errorf("craft: close runtime %q: %w", key, err))
		}
	}
	if err := c.router.Close(); err != nil {
		errs = append(errs, fmt.Errorf("craft: close router: %w", err))
	}
	if err := c.bus.Close(); err != nil {
		errs = append(errs, fmt.Errorf("craft: close bus: %w", err))
	}
	if plugins != nil {
		if err := plugins.Close(); err != nil {
			errs = append(errs, fmt.Errorf("craft: close plugins: %w", err))
		}
	}
	if err := c.closeHostMCP(); err != nil {
		errs = append(errs, fmt.Errorf("craft: close host MCP: %w", err))
	}
	return errors.Join(errs...)
}

// buildRuntime constructs one runtime through the core runtime builder.
func (c *Craft) buildRuntime(
	ctx context.Context,
	doc deploy.Document,
	externals []deploy.ExternalResource,
	resolver *resource.ReferenceResolver,
	opts RuntimeOptions,
) (*runtime.Runtime, error) {
	builder := runtime.NewBuilder(c.registry)
	if err := builder.WithLoader(c.opts.Loader); err != nil {
		return nil, fmt.Errorf("craft: runtime loader: %w", err)
	}
	if resolver != nil {
		if err := builder.WithResolver(resolver); err != nil {
			return nil, fmt.Errorf("craft: runtime resolver: %w", err)
		}
	}
	if len(externals) > 0 {
		if err := builder.WithExternalResources(
			toRuntimeExternals(externals)); err != nil {
			return nil, fmt.Errorf("craft: runtime externals: %w", err)
		}
	}
	for _, decorator := range c.hostDecorators(opts) {
		if err := builder.WithHostFactory(decorator); err != nil {
			return nil, fmt.Errorf("craft: host decorator: %w", err)
		}
	}
	for _, decorator := range c.resultHostDecorators(opts) {
		if err := builder.WithResultHostFactory(decorator); err != nil {
			return nil, fmt.Errorf("craft: result host decorator: %w", err)
		}
	}
	rt, err := builder.Build(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("craft: build runtime: %w", err)
	}
	return rt, nil
}

func (c *Craft) hostDecorators(opts RuntimeOptions) []runtime.HostFactoryDecorator {
	var out []runtime.HostFactoryDecorator
	for _, capability := range c.caps {
		if contributor, ok := capability.(HostDecorators); ok {
			out = append(out, contributor.HostFactoryDecorators()...)
		}
	}
	return append(out, opts.HostDecorators...)
}

func (c *Craft) resultHostDecorators(
	opts RuntimeOptions,
) []runtime.ResultHostFactoryDecorator {
	var out []runtime.ResultHostFactoryDecorator
	for _, capability := range c.caps {
		if contributor, ok := capability.(ResultHostDecorators); ok {
			out = append(out, contributor.ResultHostFactoryDecorators()...)
		}
	}
	return append(out, opts.ResultHostDecorators...)
}

// toRuntimeExternals converts the deploy-level external values into the
// runtime builder's injection type.
func toRuntimeExternals(
	externals []deploy.ExternalResource,
) []runtime.ExternalResource {
	out := make([]runtime.ExternalResource, 0, len(externals))
	for _, external := range externals {
		out = append(out, runtime.ExternalResource{
			ExternalDependency: runtime.ExternalDependency{
				Name:     external.Name,
				Contract: external.Contract,
			},
			Value: external.Value,
		})
	}
	return out
}

// publish emits one best-effort craft-plane event.
func (c *Craft) publish(ctx context.Context, subject event.Subject, payload any) {
	if c == nil || c.bus == nil || ptr.IsNil(ctx) {
		return
	}
	envelope, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return
	}
	_ = c.bus.Publish(ctx, envelope)
}
