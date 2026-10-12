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
	// ReloadRuntime, CloseRuntime and Close. It only ever guards the
	// table below; binder callbacks run after the mutation and outside
	// it.
	mu       sync.Mutex
	started  bool
	closed   bool
	opening  map[RuntimeKey]struct{}
	runtimes map[RuntimeKey]*runtime.Runtime
	// runtimeRecords holds, per key, what craft knows about the
	// generation that key is serving: the document it is bound to, the
	// options it was composed with and the inputs it was built with.
	// They move together, and they are what a reload that fails after
	// the runtime already swapped generations is rolled back to.
	runtimeRecords map[RuntimeKey]runtimeRecord

	// startMu serializes Start against itself. Every other mutation is
	// guarded by mu alone; Start is the one that runs long enough (it
	// spawns plugin processes) for a second caller to interleave, and
	// two concurrent starts would each bring up half the services.
	startMu sync.Mutex

	// lifecycleMu serializes the operations that must not interleave:
	// ReloadRuntime, ReloadAll and SyncAgents. mu stays the table
	// lock it has always been and is never held across a reload or an
	// agent pass; capability callbacks and event sinks those
	// operations reach run under this one.
	lifecycleMu sync.Mutex
	// agentRevs holds, per runtime key, the declaration revision
	// SyncAgents last applied for each agent name: the applied set the
	// next pass diffs against. Guarded by lifecycleMu.
	agentRevs map[RuntimeKey]map[string]string
}

// runtimeRecord is the state craft keeps for one open key: the document
// whose generation is bound and serving, the options it was composed
// with, and the inputs it was built with. The inputs are kept because a
// reload can replace them as well: the core runtime retains them once
// its generation swap commits, so the rollback of a reload whose bind
// fails has to hand the previous ones back, or the restored document
// would serve the failed call's values and externals.
type runtimeRecord struct {
	doc       deploy.Document
	opts      RuntimeOptions
	externals []deploy.ExternalResource
	resolver  *resource.ReferenceResolver
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
		def:            def,
		opts:           opts,
		caps:           caps,
		registry:       registry,
		bus:            bus,
		router:         event.NewRouter(bus),
		plugins:        opts.Plugins,
		opening:        make(map[RuntimeKey]struct{}),
		runtimes:       make(map[RuntimeKey]*runtime.Runtime),
		runtimeRecords: make(map[RuntimeKey]runtimeRecord),
		agentRevs:      make(map[RuntimeKey]map[string]string),
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

// Start starts the shared services. It is idempotent, and a failure
// leaves it retryable: nothing is marked started until the host MCP
// endpoint and the plugin host are both up.
func (c *Craft) Start(ctx context.Context) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: Start context is required")
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrCraftClosed
	}
	if c.started {
		c.mu.Unlock()
		return nil
	}
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
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
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
	if c.closed {
		// Close finished while this runtime was being composed. Publishing
		// it now would leave it running and reachable through Runtime(key)
		// on a closed Craft, so it is torn down like any other failure.
		delete(c.opening, key)
		c.mu.Unlock()
		_ = rt.Close()
		return nil, ErrCraftClosed
	}
	delete(c.opening, key)
	c.runtimes[key] = rt
	c.runtimeRecords[key] = runtimeRecord{
		doc:       doc,
		opts:      opts,
		externals: externals,
		resolver:  resolver,
	}
	c.mu.Unlock()

	if err := c.bindRuntime(ctx, key, rt); err != nil {
		_ = rt.Close()
		c.mu.Lock()
		delete(c.runtimes, key)
		delete(c.runtimeRecords, key)
		c.mu.Unlock()
		return nil, err
	}
	c.publish(ctx, SubjectRuntimeOpened, RuntimeEvent{Key: key})
	return rt, nil
}

// ReloadRuntime recomposes the layers for key and atomically replaces
// the runtime's deployment generation. In-flight turns stay on the
// previous generation. It is serialized with ReloadAll and SyncAgents.
//
// The runtime options the key was opened with — or the ones a previous
// successful ReloadRuntimeWith stored — compose the next generation;
// ReloadRuntimeWith replaces them. A closed Craft is ErrCraftClosed, an
// unknown key ErrRuntimeNotFound.
//
// Its outcome is the craft-plane pair craft.reload.started /
// .completed (or .failed), because the generation a swap installs is not
// what every caller needs. ReloadRuntimeWith is the entry point that
// returns it, and the one the failure contract is written on.
func (c *Craft) ReloadRuntime(
	ctx context.Context,
	key RuntimeKey,
	reason Reason,
) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: ReloadRuntime context is required")
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	_, err := c.reloadRuntimeLocked(ctx, key, nil, reason)
	return err
}

// ReloadRuntimeWith is ReloadRuntime with fresh runtime options: the
// next generation is composed and built with opts — new layers, Values,
// externals, host decorators — and once it is serving and bound, opts
// become the options the key holds, so a later ReloadRuntime or
// ReloadAll recomposes with them. A reload that fails leaves the key on
// the options and inputs it was serving with, and reports the same
// errors as ReloadRuntime: a bind that fails after the runtime has
// already swapped generations is rolled back to the recorded document
// rebuilt with the recorded inputs. If even that rollback fails, the
// error says which half is live — the runtime then serves the unbound
// generation the failed call built while craft's records still describe
// the previous one, so the next reload composes from that document.
//
// It exists because the per-runtime inputs are not frozen at open time
// for every application: a shell that owns a deploy overlay on disk
// re-reads it for each reload (a settings change), and without this the
// only way to hand craft the new layers would be CloseRuntime followed
// by OpenRuntime — the interruption of in-flight turns the generation
// swap exists to avoid.
//
// The returned result carries the generation the swap installed, which
// is what a caller logs or reports alongside the reload. A failed
// reload returns no result: its error says what is serving instead, and
// that is not always the generation the caller last saw — core
// allocates an id per attempt, so a rolled-back reload serves the
// previous document under a fresh id, which the error names.
func (c *Craft) ReloadRuntimeWith(
	ctx context.Context,
	key RuntimeKey,
	opts RuntimeOptions,
	reason Reason,
) (*runtime.ReloadResult, error) {
	if ptr.IsNil(ctx) {
		return nil, errdefs.Validationf("craft: ReloadRuntimeWith context is required")
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return c.reloadRuntimeLocked(ctx, key, &opts, reason)
}

// reloadRuntimeLocked is the body of both reload entry points; callers
// hold lifecycleMu. A nil opts means the options the key holds — the
// ones OpenRuntime stored, or the ones a previous successful
// ReloadRuntimeWith stored. The result is core's: the generation the
// swap installed, or nothing on failure, when the error says what serves.
func (c *Craft) reloadRuntimeLocked(
	ctx context.Context,
	key RuntimeKey,
	opts *RuntimeOptions,
	reason Reason,
) (*runtime.ReloadResult, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrCraftClosed
	}
	rt, ok := c.runtimes[key]
	if !ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	// A key in runtimes always has its record: the table and the record
	// are written and deleted together under mu.
	record, recorded := c.runtimeRecords[key]
	if opts == nil {
		stored := record.opts
		opts = &stored
	}
	c.mu.Unlock()
	c.publish(ctx, SubjectReloadStarted, RuntimeEvent{Key: key, Reason: reason})
	doc, externals, resolver, err := c.compose(ctx, key, *opts)
	if err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return nil, err
	}
	reloadOptions, err := c.reloadOptions(*opts, externals, resolver)
	if err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return nil, err
	}
	result, err := rt.Reload(ctx, doc, reloadOptions...)
	if err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return nil, fmt.Errorf("craft: reload runtime %q: %w", key, err)
	}
	if err := c.bindRuntime(ctx, key, rt); err != nil {
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return nil, c.rollbackReload(ctx, key, rt, record, recorded,
			result.GenerationID, err)
	}
	c.mu.Lock()
	// Write the generation and its inputs back only while that runtime is
	// the one this reload was for: a concurrent CloseRuntime (or
	// Close) removed the key, and a reopened key must not inherit the
	// reload's state. The whole record is replaced at once, and Close
	// swaps the table for a fresh one, so a closed Craft is left alone
	// too. A skipped write-back fails the reload below: the generation it
	// swapped belongs to a runtime the caller no longer has.
	applied := !c.closed && c.runtimes[key] == rt
	if applied {
		c.runtimeRecords[key] = runtimeRecord{
			doc:       doc,
			opts:      *opts,
			externals: externals,
			resolver:  resolver,
		}
	}
	closed := c.closed
	c.mu.Unlock()
	if !applied {
		// The key is no longer this runtime's: a concurrent CloseRuntime
		// (or Close) took it away while this reload was composing or
		// binding, so the swap it made is not the key's to serve, and
		// reporting it would name a generation nothing is bound to.
		err := fmt.Errorf("%w: %q (closed while the reload was in flight)",
			ErrRuntimeNotFound, key)
		if closed {
			err = fmt.Errorf("%w (the reload was in flight)", ErrCraftClosed)
		}
		c.publish(ctx, SubjectReloadFailed, RuntimeEvent{
			Key: key, Reason: reason, Error: err.Error()})
		return nil, err
	}
	c.publish(ctx, SubjectReloadCompleted, RuntimeEvent{Key: key, Reason: reason})
	return result, nil
}

// rollbackReload undoes a reload whose bind failed after the runtime had
// already committed the new generation, by reloading the recorded
// document — with the inputs that generation was built with, since the
// failed call's are the ones retained right now. A caller that sees an
// error then still has the document it had, which is what "failed
// reload" has to mean; the generation is the part it cannot assume,
// because core allocates an id per attempt. Every error here therefore
// names the generation that serves: installed is the id the failed swap
// installed, and the rollback's own id comes back from its swap. If even
// the rollback fails, the error says which half is live so nobody
// assumes the previous generation is serving.
func (c *Craft) rollbackReload(
	ctx context.Context,
	key RuntimeKey,
	rt *runtime.Runtime,
	record runtimeRecord,
	recorded bool,
	installed uint64,
	cause error,
) error {
	failure := fmt.Errorf("craft: reload runtime %q: bind: %w", key, cause)
	if !recorded {
		return fmt.Errorf(
			"%w (generation %d is serving but unbound; no previous "+
				"document is recorded to roll back to)", failure, installed)
	}
	options, err := c.reloadOptions(record.opts, record.externals, record.resolver)
	if err != nil {
		return fmt.Errorf(
			"%w (rolling back to the previous document failed: %v; "+
				"generation %d is serving but unbound)", failure, err, installed)
	}
	rollback, err := rt.Reload(ctx, record.doc, options...)
	if err != nil {
		return fmt.Errorf(
			"%w (rolling back to the previous document failed: %v; "+
				"generation %d is serving but unbound)", failure, err, installed)
	}
	if err := c.bindRuntime(ctx, key, rt); err != nil {
		return fmt.Errorf(
			"%w (the previous document is serving on generation %d but "+
				"rebinding it failed: %v)", failure, rollback.GenerationID, err)
	}
	return fmt.Errorf(
		"%w (the reload was rolled back; generation %d serves what the "+
			"runtime served before)", failure, rollback.GenerationID)
}

// ReloadAll reloads every open runtime. Each runtime is transactional;
// failures are aggregated with the runtime key. It holds the lifecycle
// lock across the whole pass, so neither a single-runtime reload nor an
// agent sync can interleave with it. It reports no generation — one per
// key is not one answer — so a caller that needs the id a swap installed
// reloads that key with ReloadRuntimeWith instead.
//
// It reports no error on a closed Craft: closing empties the runtime
// table, so there is nothing left to reload and the pass is a no-op —
// unlike the single-key entry points, which are keyed and report
// ErrCraftClosed. A shell that wants to know whether the pass reached
// any runtime can ask Runtimes first.
func (c *Craft) ReloadAll(ctx context.Context, reason Reason) error {
	if ptr.IsNil(ctx) {
		return errdefs.Validationf("craft: ReloadAll context is required")
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	var errs []error
	for _, key := range c.Runtimes() {
		// A nil opts means the options the key holds, which every open
		// key has; reloadRuntimeLocked reads them under mu.
		if _, err := c.reloadRuntimeLocked(ctx, key, nil, reason); err != nil {
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
		delete(c.runtimeRecords, key)
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
	plugins := c.plugins
	keys := make([]RuntimeKey, 0, len(c.runtimes))
	for key := range c.runtimes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	runtimes := c.runtimes
	c.runtimes = make(map[RuntimeKey]*runtime.Runtime)
	c.runtimeRecords = make(map[RuntimeKey]runtimeRecord)
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
	hostDecorator, err := c.hostDecorator(opts)
	if err != nil {
		return nil, err
	}
	if hostDecorator != nil {
		if err := builder.WithHostFactory(hostDecorator); err != nil {
			return nil, fmt.Errorf("craft: host decorator: %w", err)
		}
	}
	resultDecorator, err := c.resultHostDecorator(opts)
	if err != nil {
		return nil, err
	}
	if resultDecorator != nil {
		if err := builder.WithResultHostFactory(resultDecorator); err != nil {
			return nil, fmt.Errorf("craft: result host decorator: %w", err)
		}
	}
	rt, err := builder.Build(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("craft: build runtime: %w", err)
	}
	return rt, nil
}

// hostDecorator returns the host factory decorator the runtime is built
// and reloaded with, composed from the craft-wide capabilities and the
// per-runtime options. A runtime holds one decorator, so more than one
// is a validation error — the same error an open and a reload report.
func (c *Craft) hostDecorator(
	opts RuntimeOptions,
) (runtime.HostFactoryDecorator, error) {
	var out []runtime.HostFactoryDecorator
	for _, capability := range c.caps {
		if contributor, ok := capability.(HostDecorators); ok {
			out = append(out, contributor.HostFactoryDecorators()...)
		}
	}
	switch out = append(out, opts.HostDecorators...); len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0], nil
	default:
		return nil, errdefs.Validationf(
			"craft: %d host decorators: a runtime holds one", len(out))
	}
}

// resultHostDecorator is hostDecorator for the deployment-aware slot.
func (c *Craft) resultHostDecorator(
	opts RuntimeOptions,
) (runtime.ResultHostFactoryDecorator, error) {
	var out []runtime.ResultHostFactoryDecorator
	for _, capability := range c.caps {
		if contributor, ok := capability.(ResultHostDecorators); ok {
			out = append(out, contributor.ResultHostFactoryDecorators()...)
		}
	}
	switch out = append(out, opts.ResultHostDecorators...); len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0], nil
	default:
		return nil, errdefs.Validationf(
			"craft: %d result host decorators: a runtime holds one", len(out))
	}
}

// reloadOptions returns the core options that build the next generation
// of one runtime with the inputs the Craft options describe: the
// externals compose produced, the ${craft:...} resolver for the composed
// values, and the host factory decoration of the options the reload
// carries. Every channel is passed, so the generation is built with
// exactly these inputs instead of merging with the ones the previous
// generation was built with.
func (c *Craft) reloadOptions(
	opts RuntimeOptions,
	externals []deploy.ExternalResource,
	resolver *resource.ReferenceResolver,
) ([]runtime.ReloadOption, error) {
	options := []runtime.ReloadOption{
		runtime.WithReloadExternals(toRuntimeExternals(externals)),
		runtime.WithReloadResolver(resolver),
	}
	// A nil decorator is passed as such: it removes the retained one
	// instead of leaving the previous generation's decoration in place.
	hostDecorator, err := c.hostDecorator(opts)
	if err != nil {
		return nil, err
	}
	options = append(options, runtime.WithReloadHostFactory(hostDecorator))
	resultDecorator, err := c.resultHostDecorator(opts)
	if err != nil {
		return nil, err
	}
	options = append(options,
		runtime.WithReloadResultHostFactory(resultDecorator))
	return options, nil
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
