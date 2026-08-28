package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	otellog "go.opentelemetry.io/otel/log"
)

type reloadOptions struct{}

// ReloadOption configures one Reload call. The option set is reserved
// for future policy (e.g. dropping vs failing on re-bind); v1 has no
// options.
type ReloadOption func(*reloadOptions) error

// ReloadResult describes one successful generation swap.
type ReloadResult struct {
	GenerationID  uint64
	PreviousID    uint64
	ReboundAgents []string
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
	if isNilContext(ctx) {
		return nil, errdefs.Validationf(
			"runtime: Reload context is required")
	}
	for _, option := range opts {
		if isNil(option) {
			return nil, errdefs.Validationf(
				"runtime: ReloadOption must not be nil")
		}
		if err := option(&reloadOptions{}); err != nil {
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
	if r.current != nil {
		previousID = r.current.id
	}
	r.publishLifecycleEvent(ctx, SubjectRuntimeRebuildStarted(),
		RuntimeRebuildEvent{GenerationID: newGenID})
	telemetry.Info(ctx, "runtime reload started",
		otellog.Int64("runtime.generation.id", int64(newGenID)),
		otellog.Int64("runtime.generation.previous", int64(previousID)))
	fail := func(err error) (*ReloadResult, error) {
		r.publishLifecycleEvent(ctx, SubjectRuntimeRebuildFailed(),
			RuntimeRebuildEvent{GenerationID: newGenID, Error: err.Error()})
		telemetry.Error(ctx, "runtime reload failed",
			otellog.Int64("runtime.generation.id", int64(newGenID)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return fail(fmt.Errorf(
			"runtime reload validate deployment: %w", err))
	}
	cfg, err := DecodeConfig(ctx, doc)
	if err != nil {
		return fail(err)
	}

	// Build the new deployment result transactionally.
	deployBuilder := deploy.NewBuilder(r.resources)
	if r.loader != nil {
		deployBuilder = deploy.NewBuilder(
			r.resources, deploy.WithLoader(r.loader))
	}
	newResult, err := deployBuilder.Deploy(ctx, doc)
	if err != nil {
		return fail(fmt.Errorf("runtime reload build deployment: %w", err))
	}
	// drainAttempted collects every agent this Reload attempt tried to
	// remove, so an abort can clear their tombstones and leave the old
	// generation fully serving.
	var drainAttempted []string
	abort := func(err error) error {
		for _, name := range drainAttempted {
			r.manager.ReopenAgent(name)
		}
		if cerr := newResult.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "runtime reload: close partial deployment after abort", cerr,
				otellog.Int64("runtime.generation.id", int64(newGenID)))
		}
		r.publishLifecycleEvent(ctx, SubjectRuntimeRebuildFailed(),
			RuntimeRebuildEvent{GenerationID: newGenID, Error: err.Error()})
		telemetry.Error(ctx, "runtime reload aborted",
			otellog.Int64("runtime.generation.id", int64(newGenID)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return err
	}

	// Hand the runtime's session manager to the new generation's
	// consumers (e.g. the delegation service). Binding runs before the
	// swap so a failure aborts the reload without touching the live
	// generation; each generation's service instance binds exactly once.
	if err := bindSessionManagers(r.manager, newResult); err != nil {
		return nil, abort(err)
	}
	if err := bindTargetSources(r.registry, newResult); err != nil {
		return nil, abort(err)
	}

	// Resolve the new generation's system resources and validate the
	// resume contract. The bus and checkpoint store may change
	// configuration (or implementation) freely: each generation owns
	// its own values, the router subscribes to every live generation's
	// bus, and turns resolve their store from their epoch. Continuity
	// of durable session state across a store change is the host's
	// responsibility (same backing storage or migration).
	newBus, err := resolveValue[event.Bus](
		newResult, cfg.EventBus, "event_bus")
	if err != nil {
		return nil, abort(err)
	}
	var newCheckpoints agent.CheckpointStore
	if cfg.CheckpointStore != "" {
		newCheckpoints, err = resolveValue[agent.CheckpointStore](
			newResult, cfg.CheckpointStore, "checkpoint_store")
		if err != nil {
			return nil, abort(err)
		}
	}
	if cfg.Sessions.Resume {
		if newCheckpoints == nil {
			return nil, abort(errdefs.Validationf(
				"runtime reload: sessions.resume requires checkpoint_store"))
		}
		if _, ok := newCheckpoints.(agent.CheckpointDeleter); !ok {
			return nil, abort(errdefs.Validationf(
				"runtime reload: sessions.resume requires a checkpoint store " +
					"that implements CheckpointDeleter"))
		}
	}
	// Each generation must own its bus: reject a reload whose bus
	// factory returned the current generation's bus (a shared
	// singleton), detaching it from the new result so the abort path
	// cannot close a bus the old generation still routes on.
	if newBus == r.bus {
		newResult.Detach(cfg.EventBus)
		return nil, abort(errdefs.Conflictf(
			"runtime reload: event_bus factory returned the current " +
				"generation's bus; each generation must own its bus"))
	}

	// Rebuild the host factory with the same decoration policy.
	baseHostFactory, err := newBaseHostFactory(newBus, newCheckpoints)
	if err != nil {
		return nil, abort(err)
	}
	hostFactory := session.HostFactory(baseHostFactory)
	if r.hostDecorator != nil {
		hostFactory, err = r.hostDecorator(baseHostFactory)
		if err != nil {
			return nil, abort(fmt.Errorf(
				"runtime reload decorate host factory: %w", err))
		}
		if isNil(hostFactory) {
			return nil, abort(errdefs.Internalf(
				"runtime reload host factory decorator returned nil"))
		}
	}
	if r.resultHostDecorator != nil {
		hostFactory, err = r.resultHostDecorator(newResult, hostFactory)
		if err != nil {
			return nil, abort(fmt.Errorf(
				"runtime reload decorate host factory with deployment: %w", err))
		}
		if isNil(hostFactory) {
			return nil, abort(errdefs.Internalf(
				"runtime reload result host factory decorator returned nil"))
		}
	}

	// Resolve the new generation's dynamic tool catalog.
	var newCatalog *catalogRegistry
	if cfg.DynamicCatalog != nil {
		assemblies, resolveErr := resolveDynamicCatalogAssemblies(
			doc, newResult, cfg.DynamicCatalog)
		if resolveErr != nil {
			return nil, abort(resolveErr)
		}
		newCatalog = newCatalogRegistry(assemblies)
	}

	// Re-bind every dynamic agent against the new generation. Any
	// failure closes the partially bound instances and aborts.
	entries := r.registry.Entries()
	rebound := make(map[string]*agent.Agent, len(entries))
	newEntries := make(map[string]dynamicAgentEntry, len(entries))
	closeRebound := func() {
		for _, name := range sortedKeys(rebound) {
			if cerr := rebound[name].Close(); cerr != nil {
				telemetry.WarnErr(ctx, "runtime reload: close rebound agent after abort", cerr,
					otellog.String(telemetry.AttrAgentID, name),
					otellog.Int64("runtime.generation.id", int64(newGenID)))
			}
		}
	}
	for _, name := range sortedKeys(entries) {
		entry := entries[name]
		instance, bindErr := deploy.BindAgent(
			ctx, r.resources, newResult, r.loader, nil, nil, name, entry.definition)
		if bindErr != nil {
			closeRebound()
			return nil, abort(fmt.Errorf(
				"runtime reload: re-bind agent %q: %w", name, bindErr))
		}
		rebound[name] = instance
		if entry.toolAssembly != "" {
			if newCatalog == nil {
				closeRebound()
				return nil, abort(errdefs.Validationf(
					"runtime reload: agent %q has tool assembly %q but "+
						"the new document has no dynamic_catalog",
					name, entry.toolAssembly))
			}
			value, ok := newResult.Value(entry.toolAssembly)
			if !ok {
				closeRebound()
				return nil, abort(errdefs.NotFoundf(
					"runtime reload: tool assembly resource %q for agent %q not found",
					entry.toolAssembly, name))
			}
			assembly, typeOK := value.(*tool.Assembly)
			if !typeOK || assembly == nil {
				closeRebound()
				return nil, abort(errdefs.Validationf(
					"runtime reload: tool assembly resource %q for agent %q is %T, want *tool.Assembly",
					entry.toolAssembly, name, value))
			}
			newCatalog.Set(name, assembly)
		} else if newCatalog != nil && !newCatalog.hasDefault() {
			closeRebound()
			return nil, abort(errdefs.Validationf(
				"runtime reload: dynamic catalog has no default; "+
					"agent %q needs WithToolAssembly", name))
		}
		newEntries[name] = dynamicAgentEntry{
			instance:     instance,
			definition:   entry.definition,
			toolAssembly: entry.toolAssembly,
		}
	}

	// Drain sessions of deployed agents the new document removes, then
	// clear tombstones for agents that remain (or are re-added).
	var drained []string
	for _, name := range r.result.AgentNames() {
		if _, still := doc.Agents[name]; still {
			continue
		}
		drainAttempted = append(drainAttempted, name)
		if err := r.manager.RemoveAgent(ctx, name); err != nil {
			closeRebound()
			return nil, abort(fmt.Errorf(
				"runtime reload: drain removed agent %q: %w", name, err))
		}
		drained = append(drained, name)
	}
	for name := range doc.Agents {
		r.manager.ReopenAgent(name)
	}

	// Apply the new session tunables.
	if err := r.manager.UpdateTunables(session.Tunables{
		IdleTimeout:         cfg.Sessions.IdleTimeout,
		SinkBuffer:          cfg.Sessions.SinkBuffer,
		SpeculativeEvents:   cfg.Sessions.SpeculativeBufferEvents,
		SpeculativeBytes:    cfg.Sessions.SpeculativeBufferBytes,
		DeliveryConcurrency: cfg.Sessions.DeliveryConcurrency,
		MaxSessions:         cfg.Sessions.MaxSessions,
	}); err != nil {
		closeRebound()
		return nil, abort(err)
	}

	// Freeze the retiring generation, swap the live view, and commit.
	oldGen := r.current
	if oldGen != nil {
		adopted := dynamicInstances(entries)
		oldGen.freeze(adopted)
		freezeTargetViews(oldGen.result, adopted)
	}
	newGen := &Generation{
		id:          newGenID,
		doc:         doc,
		registry:    r.registry,
		result:      newResult,
		bus:         newBus,
		hostFactory: hostFactory,
		resolver: generationResolver{
			registry: r.registry,
			result:   newResult,
		},
		catalog: newCatalog,
	}
	r.registry.Replace(newEntries, newResult)
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
	if err := r.router.AddBus(newBus); err != nil {
		if oldGen != nil {
			oldGen.unfreeze()
			unfreezeTargetViews(oldGen.result)
		}
		// Belt and braces: AddBus is all-or-nothing, so the new bus is
		// never attached on error; removing it here keeps the invariant
		// even if a future router change makes AddBus partial.
		if rerr := r.router.RemoveBus(newBus); rerr != nil {
			telemetry.WarnErr(ctx, "runtime reload: remove unattached bus after abort", rerr,
				otellog.Int64("runtime.generation.id", int64(newGenID)))
		}
		r.registry.Replace(entries, oldGen.result)
		closeRebound()
		return nil, abort(fmt.Errorf("runtime reload attach bus: %w", err))
	}
	deps := session.Deps{
		Resolver:    newGen.resolver,
		HostFactory: hostFactory,
		Checkpoints: newCheckpoints,
		Resume:      cfg.Sessions.Resume,
	}
	if newGen.catalog != nil {
		// Assign through the concrete pointer so a nil catalog stays a
		// nil interface instead of a typed-nil.
		deps.CatalogProvider = newGen.catalog
	}
	if err := r.manager.SwapDeps(deps, func(_ uint64, _ session.Deps) {
		if oldGen != nil && oldGen.bus != nil {
			if rerr := r.router.RemoveBus(oldGen.bus); rerr != nil {
				telemetry.WarnErr(ctx, "runtime reload: unsubscribe retired generation bus", rerr,
					otellog.Int64("runtime.generation.id", int64(newGenID)))
			}
		}
		if cerr := oldGen.close(); cerr != nil {
			telemetry.WarnErr(ctx, "runtime reload: close retired generation", cerr,
				otellog.Int64("runtime.generation.id", int64(newGenID)))
		}
	}); err != nil {
		// Unreachable under lifecycleMu (Close holds it); roll back.
		if oldGen != nil {
			oldGen.unfreeze()
			unfreezeTargetViews(oldGen.result)
		}
		if rerr := r.router.RemoveBus(newBus); rerr != nil {
			telemetry.WarnErr(ctx, "runtime reload: remove new bus after swap failure", rerr,
				otellog.Int64("runtime.generation.id", int64(newGenID)))
		}
		r.registry.Replace(entries, oldGen.result)
		closeRebound()
		return nil, abort(errdefs.Internalf(
			"runtime reload swap: %v", err))
	}

	r.current = newGen
	r.result = newResult
	r.liveCatalog = newCatalog
	r.bus = newBus
	r.nextGenID = newGenID

	reboundNames := sortedKeys(newEntries)
	sort.Strings(drained)
	r.publishLifecycleEvent(ctx, SubjectRuntimeRebuildCompleted(),
		RuntimeRebuildEvent{
			GenerationID:         newGenID,
			PreviousGenerationID: previousID,
			ReboundAgents:        reboundNames,
			DrainedAgents:        drained,
		})
	telemetry.Info(ctx, "runtime reload completed",
		otellog.Int64("runtime.generation.id", int64(newGenID)),
		otellog.Int64("runtime.generation.previous", int64(previousID)),
		otellog.Int("runtime.reload.rebound_agents", len(reboundNames)),
		otellog.Int("runtime.reload.drained_agents", len(drained)))
	return &ReloadResult{
		GenerationID:  newGenID,
		PreviousID:    previousID,
		ReboundAgents: reboundNames,
		DrainedAgents: drained,
	}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
