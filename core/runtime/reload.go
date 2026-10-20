package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/utils/ptr"

	"go.opentelemetry.io/otel/attribute"
)

// reloadOptions collects the replacements one Reload call carries. A
// field is only applied when its replace flag is set: the zero value
// (no options) leaves every build-time input of the Runtime in place.
type reloadOptions struct {
	externals        map[string]ExternalResource
	replaceExternals bool
	resolver         *resource.ReferenceResolver
	replaceResolver  bool
	hostDecorator    HostFactoryDecorator
	replaceHost      bool
	resultDecorator  ResultHostFactoryDecorator
	replaceResult    bool
}

// ReloadOption configures one Reload call. The options replace the
// inputs the new generation is built with — the injected externals, the
// expansion resolver, the host factory decorators — when they differ
// from the ones the Runtime was built with or a previous reload
// installed. A successful reload keeps the replacement as the Runtime's
// input (a later Reload without the option builds with it); an aborted
// reload changes nothing.
type ReloadOption func(*reloadOptions) error

// WithReloadExternals replaces the injected external dependency values
// the generation this Reload builds is assembled with. The set is
// replaced, not merged: nil or empty means the generation is built with
// no externals. Names, contracts and values are validated exactly as in
// [Builder.WithExternalResources], but a name the Runtime was not built
// with is allowed — this is the option that injects it. External values
// stay caller-owned: the Runtime neither wires nor closes them.
func WithReloadExternals(externals []ExternalResource) ReloadOption {
	return func(o *reloadOptions) error {
		validated, err := validateExternalResourceList(externals)
		if err != nil {
			return err
		}
		o.externals = validated
		o.replaceExternals = true
		return nil
	}
}

// WithReloadResolver replaces the expansion resolver the generation this
// Reload builds is expanded with, for every resource, agent engine and
// hook settings subtree. A nil resolver builds the generation with the
// built-in schemes only, which is what a Runtime built without
// [Builder.WithResolver] expands with.
func WithReloadResolver(resolver *resource.ReferenceResolver) ReloadOption {
	return func(o *reloadOptions) error {
		o.resolver = resolver
		o.replaceResolver = true
		return nil
	}
}

// WithReloadHostFactory replaces the host factory decorator the
// generation this Reload builds is served with. Unlike
// [Builder.WithHostFactory], a nil decorator is meaningful: the
// generation runs with the base host factory and no decoration.
func WithReloadHostFactory(decorator HostFactoryDecorator) ReloadOption {
	return func(o *reloadOptions) error {
		o.hostDecorator = decorator
		o.replaceHost = true
		return nil
	}
}

// WithReloadResultHostFactory replaces the deployment-aware host factory
// decorator the generation this Reload builds is served with. A nil
// decorator removes the decoration. See [Builder.WithResultHostFactory].
func WithReloadResultHostFactory(decorator ResultHostFactoryDecorator) ReloadOption {
	return func(o *reloadOptions) error {
		o.resultDecorator = decorator
		o.replaceResult = true
		return nil
	}
}

// ReloadResult describes one successful generation swap.
type ReloadResult struct {
	// GenerationID is the generation serving after the swap. Ids are
	// allocated per attempt — a reload that fails before the swap
	// consumes one too — so they are monotonic but not contiguous:
	// PreviousID need not equal the id an earlier successful call
	// reported, and a caller correlates by the ids it is told, never by
	// counting.
	GenerationID uint64
	// PreviousID is the generation GenerationID replaced, or 0 when the
	// runtime had no generation to replace.
	PreviousID uint64
	// ReboundAgents names the dynamic agents re-bound against the new
	// generation, sorted by name.
	ReboundAgents []string
	// DrainedAgents names the deployed agents the new document removed,
	// whose sessions were drained, sorted by name.
	DrainedAgents []string
}

// Reload transactionally replaces the deployment document with a new
// generation:
//
//  1. Validates the document and decodes the runtime config.
//  2. Builds the new deploy.Result (rollback on failure).
//  3. Resolves the new generation's event_bus / checkpoint_store and
//     validates the resume contract. Each generation owns its values,
//     so the document may change their configuration or implementation
//     freely; a reload whose event_bus factory returns the current
//     generation's bus (a shared singleton) is rejected.
//  4. Rebuilds the host factory with the same decorator.
//  5. Re-binds every dynamic agent against the new result; any failure
//     aborts the whole reload.
//  6. Drains sessions of deployed agents removed by the new document.
//  7. Atomically swaps the manager epoch and the runtime's current
//     generation; the old generation retires and closes once its
//     in-flight turns drain.
//
// The returned result names what the committed swap did: the generation
// now serving, the one it replaced, and the agents it re-bound and
// drained. An aborted reload returns no result.
//
// The generation can be built with replacement inputs through
// ReloadOptions (WithReloadExternals, WithReloadResolver,
// WithReloadHostFactory, WithReloadResultHostFactory). Each option
// replaces the Runtime's corresponding build-time input for the new
// generation and, once the swap commits, is retained — a later Reload
// without the option builds with the replacement. An aborted reload
// leaves every retained input untouched.
//
// In-flight turns always complete on the generation they started on;
// the next Start uses the new generation. Reload is serialized with
// RegisterAgent / UnregisterAgent / Close via lifecycleMu.
func (r *Runtime) Reload(
	ctx context.Context,
	doc deploy.Document,
	opts ...ReloadOption,
) (*ReloadResult, error) {
	if r == nil {
		return nil, errdefs.Validationf(
			"runtime: Reload requires a built Runtime")
	}
	if ptr.IsNil(ctx) {
		return nil, errdefs.Validationf(
			"runtime: Reload context is required")
	}
	override := &reloadOptions{}
	for _, option := range opts {
		if ptr.IsNil(option) {
			return nil, errdefs.Validationf(
				"runtime: ReloadOption must not be nil")
		}
		if err := option(override); err != nil {
			return nil, err
		}
	}

	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return nil, errdefs.NotAvailablef("runtime: closed")
	}
	// Allocate the attempt's generation id up front and publish the
	// started event before any validation so every attempt (success or
	// failure) has a complete started -> completed/failed chain with a
	// distinct id.
	newGenID := r.nextGenID + 1
	r.nextGenID = newGenID
	previousID := uint64(0)
	if current := r.current.Load(); current != nil {
		previousID = current.id
	}
	r.publishLifecycleEvent(ctx, SubjectRuntimeRebuildStarted(),
		RuntimeRebuildEvent{GenerationID: newGenID})
	telemetry.Info(ctx, "runtime reload started",
		attribute.Int64("runtime.generation.id", int64(newGenID)),
		attribute.Int64("runtime.generation.previous", int64(previousID)))
	return (&reloadAttempt{
		r:        r,
		ctx:      ctx,
		doc:      doc,
		override: override,
		newGenID: newGenID,
		previous: previousID,
	}).run()
}

// reloadAttempt is one Reload call: the generation ids it was allocated, the
// inputs it builds from, and the partial deployment an abort must unwind.
// Each phase of the reload is a method that either takes the attempt one step
// further or returns the error the caller sees, so the failure paths sit at
// the top of run() instead of being interleaved with the happy path.
type reloadAttempt struct {
	r        *Runtime
	ctx      context.Context
	doc      deploy.Document
	override *reloadOptions
	newGenID uint64
	previous uint64

	// Settled by prepare: the build-time inputs this generation starts
	// from, after the reload's options replaced what they cover.
	cfg                 Config
	injected            map[string]ExternalResource
	resolver            *resource.ReferenceResolver
	hostDecorator       HostFactoryDecorator
	resultHostDecorator ResultHostFactoryDecorator
	effective           []ExternalResource

	// Settled by build, bind, resolveValues and resolveCatalog: the new
	// deployment and the values the new generation serves.
	result      *deploy.Result
	bus         event.Bus
	checkpoints agent.CheckpointStore
	hostFactory session.HostFactory
	catalog     *catalogRegistry

	// Settled by rebindAgents and drainRemovedAgents: what the commit
	// installs and what the completed event reports.
	entries        map[string]dynamicAgentEntry
	rebound        map[string]*agent.Agent
	newEntries     map[string]dynamicAgentEntry
	drainAttempted []string
	drained        []string
}

// run executes the reload one phase at a time. Nothing is visible to
// callers until commit swaps the live generation; every earlier failure
// aborts the attempt instead.
func (a *reloadAttempt) run() (*ReloadResult, error) {
	// Until the deployment exists there is nothing to unwind; afterwards an
	// abort also closes what the attempt built.
	if err := a.prepare(); err != nil {
		return nil, a.reject(err)
	}
	if err := a.build(); err != nil {
		return nil, a.reject(err)
	}
	if err := a.bind(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.resolveValues(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.resolveHostFactory(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.resolveCatalog(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.rebindAgents(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.drainRemovedAgents(); err != nil {
		return nil, a.abort(err)
	}
	if err := a.commit(); err != nil {
		return nil, a.abort(err)
	}
	return a.committed(), nil
}

// prepare validates the document and settles the build-time inputs this
// generation starts from: the retained ones, minus whatever the reload's
// options replace.
func (a *reloadAttempt) prepare() error {
	if err := a.doc.Validate(); err != nil {
		return fmt.Errorf("runtime reload validate deployment: %w", err)
	}
	cfg, err := DecodeConfig(a.ctx, a.doc)
	if err != nil {
		return err
	}
	a.cfg = cfg
	// The generation's externals, resolver and host decorations are the
	// retained ones unless this reload replaces them.
	a.injected = a.r.externalResources
	if a.override.replaceExternals {
		a.injected = a.override.externals
	}
	a.resolver = a.r.resolver
	if a.override.replaceResolver {
		a.resolver = a.override.resolver
	}
	a.hostDecorator = a.r.hostDecorator
	if a.override.replaceHost {
		a.hostDecorator = a.override.hostDecorator
	}
	a.resultHostDecorator = a.r.resultHostDecorator
	if a.override.replaceResult {
		a.resultHostDecorator = a.override.resultDecorator
	}
	effective, err := effectiveExternalResources(a.cfg.ExternalDeps, a.injected, true)
	if err != nil {
		return err
	}
	a.effective = effective
	return nil
}

// build constructs the new deployment result transactionally.
func (a *reloadAttempt) build() error {
	var deployOptions []deploy.BuilderOption
	if len(a.effective) > 0 {
		deployValues := make([]deploy.ExternalResource, 0, len(a.effective))
		for _, ext := range a.effective {
			deployValues = append(deployValues, toDeployExternalResource(ext))
		}
		deployOptions = append(deployOptions, deploy.WithExternalResources(deployValues))
	}
	if a.r.loader != nil {
		deployOptions = append(deployOptions, deploy.WithLoader(a.r.loader))
	}
	if a.resolver != nil {
		deployOptions = append(deployOptions, deploy.WithResolver(a.resolver))
	}
	result, err := deploy.NewBuilder(a.r.resources, deployOptions...).Deploy(a.ctx, a.doc)
	if err != nil {
		return fmt.Errorf("runtime reload build deployment: %w", err)
	}
	a.result = result
	return nil
}

// bind hands the runtime's session manager to the new generation's consumers
// (e.g. the delegation service) and rebinds the target sources. Binding runs
// before the swap so a failure aborts the reload without touching the live
// generation; each generation's service instance binds exactly once.
func (a *reloadAttempt) bind() error {
	if err := bindSessionManagers(a.r.manager, a.result); err != nil {
		return err
	}
	return bindTargetSources(a.r.registry, a.result)
}

// resolveValues resolves the new generation's system resources and validates
// the resume contract. The bus and checkpoint store may change configuration
// (or implementation) freely: each generation owns its own values, the router
// subscribes to every live generation's bus, and turns resolve their store
// from their epoch. Continuity of durable session state across a store change
// is the host's responsibility (same backing storage or migration).
func (a *reloadAttempt) resolveValues() error {
	bus, err := resolveValue[event.Bus](a.result, a.cfg.EventBus, "event_bus")
	if err != nil {
		return err
	}
	if a.cfg.CheckpointStore != "" {
		checkpoints, resolveErr := resolveValue[agent.CheckpointStore](
			a.result, a.cfg.CheckpointStore, "checkpoint_store")
		if resolveErr != nil {
			return resolveErr
		}
		a.checkpoints = checkpoints
	}
	if a.cfg.Sessions.Resume {
		if a.checkpoints == nil {
			return errdefs.Validationf(
				"runtime reload: sessions.resume requires checkpoint_store")
		}
		if _, ok := a.checkpoints.(agent.CheckpointDeleter); !ok {
			return errdefs.Validationf(
				"runtime reload: sessions.resume requires a checkpoint store " +
					"that implements CheckpointDeleter")
		}
	}
	// Each generation must own its bus: reject a reload whose bus factory
	// returned the current generation's bus (a shared singleton), detaching
	// it from the new result so the abort path cannot close a bus the old
	// generation still routes on.
	if bus == a.r.bus {
		a.result.Detach(a.cfg.EventBus)
		return errdefs.Conflictf(
			"runtime reload: event_bus factory returned the current " +
				"generation's bus; each generation must own its bus")
	}
	a.bus = bus
	return nil
}

// resolveHostFactory rebuilds the host factory with this generation's
// decoration policy.
func (a *reloadAttempt) resolveHostFactory() error {
	baseHostFactory, err := newBaseHostFactory(a.bus, a.checkpoints)
	if err != nil {
		return err
	}
	hostFactory := session.HostFactory(baseHostFactory)
	if a.hostDecorator != nil {
		hostFactory, err = a.hostDecorator(baseHostFactory)
		if err != nil {
			return fmt.Errorf("runtime reload decorate host factory: %w", err)
		}
		if ptr.IsNil(hostFactory) {
			return errdefs.Internalf(
				"runtime reload host factory decorator returned nil")
		}
	}
	if a.resultHostDecorator != nil {
		hostFactory, err = a.resultHostDecorator(a.result, hostFactory)
		if err != nil {
			return fmt.Errorf(
				"runtime reload decorate host factory with deployment: %w", err)
		}
		if ptr.IsNil(hostFactory) {
			return errdefs.Internalf(
				"runtime reload result host factory decorator returned nil")
		}
	}
	a.hostFactory = hostFactory
	return nil
}

// resolveCatalog resolves the new generation's dynamic tool catalog. A
// document without one leaves the generation without a catalog, which
// rebindAgents rejects for any agent that declares a tool assembly.
func (a *reloadAttempt) resolveCatalog() error {
	if a.cfg.DynamicCatalog == nil {
		return nil
	}
	assemblies, err := resolveDynamicCatalogAssemblies(
		a.doc, a.result, a.cfg.DynamicCatalog)
	if err != nil {
		return err
	}
	a.catalog = newCatalogRegistry(assemblies)
	return nil
}

// rebindAgents re-binds every dynamic agent against the new generation. Any
// failure closes the partially bound instances and aborts.
func (a *reloadAttempt) rebindAgents() error {
	a.entries = a.r.registry.Entries()
	a.rebound = make(map[string]*agent.Agent, len(a.entries))
	a.newEntries = make(map[string]dynamicAgentEntry, len(a.entries))
	for _, name := range sortedKeys(a.entries) {
		if err := a.rebindAgent(name); err != nil {
			a.closeRebound()
			return err
		}
	}
	return nil
}

// rebindAgent binds one dynamic agent to the new generation and records it
// for the commit.
func (a *reloadAttempt) rebindAgent(name string) error {
	entry := a.entries[name]
	instance, err := deploy.BindAgent(
		a.ctx, a.r.resources, a.result, a.r.loader,
		a.result.Resolver(), a.result.Secrets(), name, entry.definition)
	if err != nil {
		return fmt.Errorf("runtime reload: re-bind agent %q: %w", name, err)
	}
	a.rebound[name] = instance
	if entry.toolAssembly != "" {
		if a.catalog == nil {
			return errdefs.Validationf(
				"runtime reload: agent %q has tool assembly %q but "+
					"the new document has no dynamic_catalog",
				name, entry.toolAssembly)
		}
		assembly, resolveErr := deploy.Resolve[*tool.Assembly](a.result, entry.toolAssembly)
		if resolveErr != nil {
			return fmt.Errorf("runtime reload: agent %q: %w", name, resolveErr)
		}
		a.catalog.Set(name, assembly)
	} else if a.catalog != nil && !a.catalog.hasDefault() {
		return errdefs.Validationf(
			"runtime reload: dynamic catalog has no default; "+
				"agent %q needs WithToolAssembly", name)
	}
	a.newEntries[name] = dynamicAgentEntry{
		instance:     instance,
		definition:   entry.definition,
		toolAssembly: entry.toolAssembly,
	}
	return nil
}

// drainRemovedAgents drains the sessions of deployed agents the new document
// removes, clears tombstones for the agents that remain (or are re-added),
// and applies the new session tunables.
func (a *reloadAttempt) drainRemovedAgents() error {
	for _, name := range a.r.result.AgentNames() {
		if _, still := a.doc.Agents[name]; still {
			continue
		}
		a.drainAttempted = append(a.drainAttempted, name)
		if err := a.r.manager.RemoveAgent(a.ctx, name); err != nil {
			a.closeRebound()
			return fmt.Errorf("runtime reload: drain removed agent %q: %w", name, err)
		}
		a.drained = append(a.drained, name)
	}
	for name := range a.doc.Agents {
		a.r.manager.ReopenAgent(name)
	}
	if err := a.r.manager.UpdateTunables(session.Tunables{
		IdleTimeout:         a.cfg.Sessions.IdleTimeout,
		SinkBuffer:          a.cfg.Sessions.SinkBuffer,
		SpeculativeEvents:   a.cfg.Sessions.SpeculativeBufferEvents,
		SpeculativeBytes:    a.cfg.Sessions.SpeculativeBufferBytes,
		DeliveryConcurrency: a.cfg.Sessions.DeliveryConcurrency,
		MaxSessions:         a.cfg.Sessions.MaxSessions,
	}); err != nil {
		a.closeRebound()
		return err
	}
	return nil
}

// commit freezes the retiring generation, swaps the live view, and makes the
// new generation current. Every rollback it needs on failure is applied
// before it returns, so the caller only has to abort.
func (a *reloadAttempt) commit() error {
	oldGen := a.r.current.Load()
	if oldGen != nil {
		adopted := dynamicInstances(a.entries)
		oldGen.freeze(adopted)
		freezeTargetViews(oldGen.result, adopted)
	}
	newGen := &Generation{
		id:          a.newGenID,
		doc:         a.doc,
		registry:    a.r.registry,
		result:      a.result,
		bus:         a.bus,
		hostFactory: a.hostFactory,
		resolver: generationResolver{
			registry: a.r.registry,
			result:   a.result,
		},
		catalog:            a.catalog,
		agentRemoveTimeout: a.cfg.Agents.RemoveTimeout,
	}
	a.r.registry.Replace(a.newEntries, a.result)
	// The live view is swapped before the epoch swap on purpose: the
	// new generation's resolver serves dynamic agents from the live
	// registry, so ordering it after SwapDeps would expose a window
	// where a new-epoch turn resolves an old-generation instance. The
	// early Replace only means discovery readers may briefly observe
	// the new (fully valid) instances before the epoch commits; all
	// mutation paths are serialized by lifecycleMu.
	// Route the new generation's bus through the runtime router before
	// the swap so the first new-generation turns are streamed; the old
	// bus is unsubscribed when its generation retires.
	if err := a.r.router.AddBus(a.bus); err != nil {
		a.rollbackTo(oldGen, "runtime reload: remove unattached bus after abort")
		return fmt.Errorf("runtime reload attach bus: %w", err)
	}
	deps := session.Deps{
		Resolver:    newGen.resolver,
		HostFactory: a.hostFactory,
		Checkpoints: a.checkpoints,
		Resume:      a.cfg.Sessions.Resume,
	}
	if newGen.catalog != nil {
		// Assign through the concrete pointer so a nil catalog stays a
		// nil interface instead of a typed-nil.
		deps.CatalogProvider = newGen.catalog
	}
	if err := a.r.manager.SwapDeps(deps, func(_ uint64, _ session.Deps) {
		a.retire(oldGen)
	}); err != nil {
		// Unreachable under lifecycleMu (Close holds it); roll back.
		a.rollbackTo(oldGen, "runtime reload: remove new bus after swap failure")
		return errdefs.Internalf("runtime reload swap: %v", err)
	}

	a.r.current.Store(newGen)
	a.r.result = a.result
	a.r.liveCatalog = a.catalog
	a.r.bus = a.bus
	a.r.nextGenID = a.newGenID
	a.retainInputs()
	return nil
}

// retire unsubscribes and closes the generation the swap replaced. It runs
// inside the manager's swap callback, once no new turn can resolve the old
// epoch.
func (a *reloadAttempt) retire(oldGen *Generation) {
	if oldGen != nil && oldGen.bus != nil {
		if err := a.r.router.RemoveBus(oldGen.bus); err != nil {
			telemetry.WarnErr(a.ctx, "runtime reload: unsubscribe retired generation bus", err,
				attribute.Int64("runtime.generation.id", int64(a.newGenID)))
		}
	}
	if err := oldGen.close(); err != nil {
		telemetry.WarnErr(a.ctx, "runtime reload: close retired generation", err,
			attribute.Int64("runtime.generation.id", int64(a.newGenID)))
	}
}

// rollbackTo undoes the parts of commit that already took effect: the
// retiring generation unfreezes and serves again, the new bus detaches (AddBus
// is all-or-nothing, so this keeps the invariant even if a future router
// change makes it partial), the live registry returns to the old generation's
// entries, and the agents this attempt bound are closed.
//
// busRemoveMsg names the rollback site in the log line that reports a bus that
// would not detach, so an operator can tell an AddBus failure from a swap
// failure.
func (a *reloadAttempt) rollbackTo(oldGen *Generation, busRemoveMsg string) {
	if oldGen != nil {
		oldGen.unfreeze()
		unfreezeTargetViews(oldGen.result)
	}
	if err := a.r.router.RemoveBus(a.bus); err != nil {
		telemetry.WarnErr(a.ctx, busRemoveMsg, err,
			attribute.Int64("runtime.generation.id", int64(a.newGenID)))
	}
	if oldGen != nil {
		a.r.registry.Replace(a.entries, oldGen.result)
	}
	a.closeRebound()
}

// retainInputs makes this generation's build-time inputs the retained ones: a
// later Reload without the option builds with exactly what this generation was
// built with.
func (a *reloadAttempt) retainInputs() {
	if a.override.replaceExternals {
		a.r.externalResources = a.override.externals
	}
	if a.override.replaceResolver {
		a.r.resolver = a.override.resolver
	}
	if a.override.replaceHost {
		a.r.hostDecorator = a.override.hostDecorator
	}
	if a.override.replaceResult {
		a.r.resultHostDecorator = a.override.resultDecorator
	}
}

// closeRebound closes the agents this attempt bound, so a failed reload never
// leaks live instances.
func (a *reloadAttempt) closeRebound() {
	for _, name := range sortedKeys(a.rebound) {
		if err := a.rebound[name].Close(); err != nil {
			telemetry.WarnErr(a.ctx, "runtime reload: close rebound agent after abort", err,
				attribute.String(telemetry.AttrAgentID, name),
				attribute.Int64("runtime.generation.id", int64(a.newGenID)))
		}
	}
}

// committed reports the swap that took effect.
func (a *reloadAttempt) committed() *ReloadResult {
	reboundNames := sortedKeys(a.newEntries)
	sort.Strings(a.drained)
	a.r.publishLifecycleEvent(a.ctx, SubjectRuntimeRebuildCompleted(),
		RuntimeRebuildEvent{
			GenerationID:         a.newGenID,
			PreviousGenerationID: a.previous,
			ReboundAgents:        reboundNames,
			DrainedAgents:        a.drained,
		})
	telemetry.Info(a.ctx, "runtime reload completed",
		attribute.Int64("runtime.generation.id", int64(a.newGenID)),
		attribute.Int64("runtime.generation.previous", int64(a.previous)),
		attribute.Int("runtime.reload.rebound_agents", len(reboundNames)),
		attribute.Int("runtime.reload.drained_agents", len(a.drained)))
	return &ReloadResult{
		GenerationID:  a.newGenID,
		PreviousID:    a.previous,
		ReboundAgents: reboundNames,
		DrainedAgents: a.drained,
	}
}

// reject reports a failure that never produced a deployment: nothing the
// attempt built needs unwinding.
func (a *reloadAttempt) reject(err error) error {
	a.r.publishLifecycleEvent(a.ctx, SubjectRuntimeRebuildFailed(),
		RuntimeRebuildEvent{GenerationID: a.newGenID, Error: err.Error()})
	telemetry.Error(a.ctx, "runtime reload failed",
		attribute.Int64("runtime.generation.id", int64(a.newGenID)),
		attribute.String(telemetry.AttrErrorMessage, err.Error()))
	return err
}

// abort unwinds an attempt whose deployment was already built: the agents
// whose sessions it tried to drain reopen, the partial deployment closes, and
// the failure is reported as an abort.
func (a *reloadAttempt) abort(err error) error {
	for _, name := range a.drainAttempted {
		a.r.manager.ReopenAgent(name)
	}
	if a.result != nil {
		if closeErr := a.result.Close(); closeErr != nil {
			telemetry.WarnErr(a.ctx,
				"runtime reload: close partial deployment after abort", closeErr,
				attribute.Int64("runtime.generation.id", int64(a.newGenID)))
		}
	}
	a.r.publishLifecycleEvent(a.ctx, SubjectRuntimeRebuildFailed(),
		RuntimeRebuildEvent{GenerationID: a.newGenID, Error: err.Error()})
	telemetry.Error(a.ctx, "runtime reload aborted",
		attribute.Int64("runtime.generation.id", int64(a.newGenID)),
		attribute.String(telemetry.AttrErrorMessage, err.Error()))
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
