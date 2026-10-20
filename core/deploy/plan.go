package deploy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// Plan is a deployment document checked against a registry: every kind
// resolves to a factory, every dependency a document supplies is declared by
// that factory, every required dependency is present, the resource graph is
// acyclic, and every dependency edge is type-correct.
//
// Nothing is constructed and no side effect is performed. Plan is therefore
// the place a configuration error is reported — one run lists every bad edge
// instead of the first — and the place a validator (a CLI preflight, the
// flowcraft-config skill) can run without touching the network, the
// filesystem or a sandbox.
type Plan struct {
	builder   *Builder
	doc       Document
	order     []string
	entries   map[string]resource.Entry
	externals map[string]ExternalResource

	// The builder's construction inputs are copied at plan time so that
	// mutating the Builder between Plan and Build cannot change what gets
	// built: the plan validated one configuration, and Build uses it.
	loader              *resource.Loader
	resolver            *resource.ReferenceResolver
	secretCacheTTL      time.Duration
	secretCacheCapacity int
}

// Plan validates doc against the builder's registry without constructing
// anything.
//
// Every problem is reported at once, joined into one error, because a
// deployment document is authored as a whole: fixing one dep at a time is
// the slow path for both humans and models.
func (b *Builder) Plan(doc Document) (*Plan, error) {
	if b == nil {
		return nil, errdefs.Validationf("deploy: nil builder")
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	externals, err := normalizeExternalResources(b.externalResources)
	if err != nil {
		return nil, err
	}
	graph := resource.NewGraph()
	for name, res := range doc.Resources {
		if err := graph.Add(name, res); err != nil {
			return nil, err
		}
	}
	for name := range externals {
		if err := graph.AddExternal(name); err != nil {
			return nil, err
		}
	}
	order, err := graph.TopoOrder()
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		builder:             b,
		doc:                 doc,
		order:               order,
		entries:             make(map[string]resource.Entry, len(doc.Resources)),
		externals:           externals,
		loader:              b.loader,
		resolver:            b.resolver,
		secretCacheTTL:      b.secretCacheTTL,
		secretCacheCapacity: b.secretCacheCapacity,
	}

	// Pass 1: resolve every factory. A missing factory makes its consumers
	// untypeable, so type checking waits until all of them resolve; otherwise
	// one missing kind would produce a cascade of misleading edge errors.
	var errs []error
	for _, name := range order {
		res := doc.Resources[name]
		entry, ok := b.registry.Lookup(res.Kind, res.Impl)
		if !ok {
			errs = append(errs, errdefs.Validationf(
				"resources[%q]: no factory for %s/%s", name, res.Kind, res.Impl))
			continue
		}
		plan.entries[name] = entry
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Pass 2: every edge, then every agent engine and hook slot.
	for _, name := range order {
		res := doc.Resources[name]
		entry := plan.entries[name]
		if err := checkDeps(entry.Spec(), res.Deps, plan.producerType); err != nil {
			errs = append(errs, errdefs.Validationf("resources[%q]: %v", name, err))
		}
		// A secret store is a socket the assembly itself requires: Build has to
		// use the value as a resource.SecretStore to assemble the ${secret:…}
		// scheme, so check that here rather than after constructing it.
		if res.Kind == secret.ResourceKind {
			if err := checkProduces(entry, reflect.TypeFor[resource.SecretStore](),
				fmt.Sprintf("resources[%q]", name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, plan.checkAgents()...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return plan, nil
}

// producerType resolves a dep ref to the Go type of the value it denotes:
// an external's injected type, a container's declared item type, or a
// resource factory's build type.
func (p *Plan) producerType(ref resource.Ref) (reflect.Type, error) {
	name := ref.ResourceName()
	if ext, ok := p.externals[name]; ok {
		if _, hasItem := ref.ItemName(); hasItem {
			return nil, errdefs.Validationf(
				"external %q does not support item references", name)
		}
		if ext.Type == nil {
			return nil, errdefs.Validationf(
				"external %q was injected without a Go type", name)
		}
		return ext.Type, nil
	}
	entry, ok := p.entries[name]
	if !ok {
		return nil, errdefs.NotFoundf(
			"ref %q matches no resource and no external", string(ref))
	}
	if _, hasItem := ref.ItemName(); hasItem {
		items := entry.Spec().Items
		if items == nil {
			return nil, errdefs.Validationf(
				"resource %q does not expose items (ref %q)", name, string(ref))
		}
		return items, nil
	}
	return entry.BuildType(), nil
}

// checkAgents validates every agent's engine and hook slots with the same
// edge rules used for resources.
func (p *Plan) checkAgents() []error {
	names := make([]string, 0, len(p.doc.Agents))
	for name := range p.doc.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		def := p.doc.Agents[name]
		entry, ok := p.builder.registry.Lookup(def.Engine.Kind, def.Engine.Impl)
		if !ok {
			errs = append(errs, errdefs.Validationf(
				"agents[%q]: no factory for engine %s/%s",
				name, def.Engine.Kind, def.Engine.Impl))
			continue
		}
		if err := checkProduces(entry, reflect.TypeFor[agent.Engine](),
			fmt.Sprintf("agents[%q].engine", name)); err != nil {
			errs = append(errs, err)
		}
		if err := checkDeps(entry.Spec(), def.Engine.Deps, p.producerType); err != nil {
			errs = append(errs, errdefs.Validationf("agents[%q].engine: %v", name, err))
		}
		for _, slot := range hookSlots(def) {
			for i, hook := range slot.entries {
				where := fmt.Sprintf("agents[%q].%s[%d]", name, slot.slot, i)
				entry, ok := p.builder.registry.Lookup(resource.Kind("hook."+slot.slot), hook.Type)
				if !ok {
					errs = append(errs, errdefs.Validationf(
						"%s: no factory for hook.%s/%s", where, slot.slot, hook.Type))
					continue
				}
				if err := checkProduces(entry, slot.want, where); err != nil {
					errs = append(errs, err)
				}
				if err := checkDeps(entry.Spec(), hook.Deps, p.producerType); err != nil {
					errs = append(errs, errdefs.Validationf("%s: %v", where, err))
				}
			}
		}
	}
	return errs
}

// effectiveResolver returns the expansion resolver for the main build
// pass: the all-open default resolver merged with the builder's
// injected schemes, plus the ${secret:...} scheme assembled from the
// built stores (each wrapped in a TTL cache for per-request
// resolution). The second return is the resolver handed to factories
// for lazy Secret.Resolve calls.
func (p *Plan) effectiveResolver(
	stores map[string]resource.SecretStore,
	defaultStore string,
) (*resource.ReferenceResolver, *resource.SecretResolver, error) {
	base := defaultExpansionResolver(p.loader)
	if p.resolver != nil {
		base = base.Merge(p.resolver)
	}
	if len(stores) == 0 {
		return base, nil, nil
	}
	cached := make(map[string]resource.SecretStore, len(stores))
	ttl := p.secretCacheTTL
	if ttl == 0 {
		ttl = defaultSecretCacheTTL
	}
	for id, store := range stores {
		cached[id] = resource.NewCachingSecretStore(store, ttl,
			resource.WithCacheCapacity(p.secretCacheCapacity))
	}
	secrets := resource.NewSecretResolver(cached, defaultStore)
	return base.WithScheme(secrets.Scheme()), secrets, nil
}

// Build constructs every resource and binds every agent from the checked
// plan. On failure, values already built are closed in reverse construction
// order (rollback).
func (p *Plan) Build(ctx context.Context) (*Result, error) {
	doc := p.doc
	order := p.order
	externals := p.externals

	values := make(map[string]any, len(order))
	// Secret stores are built first: their settings never reference
	// secrets, and once assembled they feed the ${secret:...} scheme
	// every other resource's expansion uses.
	stores, defaultStore, err := p.buildSecretStores(ctx, order, values)
	if err != nil {
		logCleanup(ctx, values, order)
		return nil, err
	}
	resolver, secrets, err := p.effectiveResolver(stores, defaultStore)
	if err != nil {
		logCleanup(ctx, values, order)
		return nil, err
	}
	for _, name := range order {
		if _, prebuilt := values[name]; prebuilt {
			continue
		}
		value, err := p.buildResource(ctx, name, values, resolver, secrets)
		if err != nil {
			logCleanup(ctx, values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		values[name] = value
	}

	return &Result{
		values:    values,
		order:     order,
		agents:    make(map[string]*agent.Agent, len(doc.Agents)),
		resolver:  resolver,
		secrets:   secrets,
		externals: externals,
	}, nil
}

// checkDeps validates one factory's dependency set against a document: every
// supplied key is declared by the factory, every required dep is present, and
// every resolved producer type satisfies the declared requirement.
//
// It is shared by the plan phase (before anything is constructed) and by
// [BindAgent] (for agents registered after a build), so a dependency is judged
// by exactly one rule everywhere.
func checkDeps(
	spec resource.Spec,
	deps resource.Deps,
	resolve func(resource.Ref) (reflect.Type, error),
) error {
	keys := make([]string, 0, len(deps))
	for key := range deps {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var errs []error
	supplied := make(map[string]int, len(spec.Deps))
	for _, key := range keys {
		req, declared := spec.DepSpecFor(key)
		if !declared {
			errs = append(errs, errdefs.Validationf(
				"dep %q is not declared by %s/%s", key, spec.Kind, spec.Impl))
			continue
		}
		supplied[req.Name]++
		got, err := resolve(deps[key])
		if err != nil {
			errs = append(errs, errdefs.Validationf("dep %q: %v", key, err))
			continue
		}
		if err := acceptType(req, got); err != nil {
			errs = append(errs, errdefs.Validationf("dep %q: %v", key, err))
		}
	}
	for _, req := range spec.Deps {
		if req.Required && supplied[req.Name] == 0 {
			errs = append(errs, errdefs.Validationf(
				"required dep %q is missing (want %s)", req.Name, req.Type))
		}
	}
	return errors.Join(errs...)
}

// acceptType reports whether a producer's Go type satisfies a requirement.
func acceptType(req resource.Requirement, got reflect.Type) error {
	if req.Type == nil {
		return errdefs.Validationf(
			"dep %q has no declared Go type; declare it with resource.Require/Optional", req.Name)
	}
	if got == nil {
		return errdefs.Validationf("value has no Go type, want %s", req.Type)
	}
	if isDynamic(got) {
		// A Factory[any] is dynamically typed (a plugin host forwarding a
		// value it does not model). Its contract is enforced where the value
		// is read, not here.
		return nil
	}
	if !got.AssignableTo(req.Type) {
		return errdefs.Validationf("%s does not satisfy %s", got, req.Type)
	}
	return nil
}

// checkProduces reports whether a factory's output satisfies want — used for
// the engine and hook sockets, whose expected interface is fixed by the agent
// contract rather than by a document dep.
func checkProduces(entry resource.Entry, want reflect.Type, where string) error {
	got := entry.BuildType()
	if isDynamic(got) {
		return nil
	}
	if !got.AssignableTo(want) {
		return errdefs.Validationf(
			"%s: factory %s/%s produces %s, want %s",
			where, entry.Spec().Kind, entry.Spec().Impl, got, want)
	}
	return nil
}

// isDynamic reports whether a factory declined to declare a concrete output
// type by instantiating Factory[any].
func isDynamic(t reflect.Type) bool {
	return t != nil && t.Kind() == reflect.Interface && t.NumMethod() == 0
}

// hookSlot is one agent hook socket: its document key, the interface every
// hook in it must satisfy, and the entries the agent declared.
type hookSlot struct {
	slot    string
	want    reflect.Type
	entries []agent.Hook
}

// hookSlots enumerates an agent's hook sockets in construction order. Plan,
// Wire and the failure rollback all walk the same list, so a socket cannot be
// validated in one place and built in another.
func hookSlots(def agent.Definition) []hookSlot {
	return []hookSlot{
		{slot: agent.HookSlotPreparer, want: reflect.TypeFor[agent.Preparer](), entries: def.Prepare},
		{slot: agent.HookSlotObserver, want: reflect.TypeFor[agent.Observer](), entries: def.Observe},
		{slot: agent.HookSlotReferee, want: reflect.TypeFor[agent.Referee](), entries: def.Referees},
		{slot: agent.HookSlotCommitter, want: reflect.TypeFor[agent.Committer](), entries: def.Commit},
	}
}
