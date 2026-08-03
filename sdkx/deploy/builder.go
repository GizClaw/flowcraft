package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// SourceFunc resolves one dep reference into a value the HOST owns —
// a closure over an instance built outside this document. Resolution
// happens once, at Build time, and the returned value is never
// closed by [Result.Close]: a source is borrowed, not owned.
type SourceFunc func(ctx context.Context, ref string) (any, error)

// ResourceDepSpec declares one named dependency accepted by a resource
// factory.
type ResourceDepSpec struct {
	Name     string `json:"name" yaml:"name"`
	Type     string `json:"type" yaml:"type"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

// ResourceSpec is the static declaration for one resource factory.
// Kind and Impl form its registry key. ItemType is non-empty when the
// resource is a container whose named items can be resolved.
type ResourceSpec struct {
	Kind     string            `json:"kind" yaml:"kind"`
	Impl     string            `json:"impl" yaml:"impl"`
	Deps     []ResourceDepSpec `json:"deps,omitempty" yaml:"deps,omitempty"`
	ItemType string            `json:"item_type,omitempty" yaml:"item_type,omitempty"`
}

// ResourceInput is what a resource factory receives: its own opaque
// settings subtree plus the already-built values declared by its Spec.
type ResourceInput struct {
	// Settings is the impl-owned subtree. Decode it with
	// [DecodeSettings] so unknown keys fail the build.
	Settings *yamlv3.Node

	// Deps holds resolved dependencies keyed by the names used in the
	// document. The factory type-asserts concrete Go values itself.
	Deps map[string]any
}

// Dep returns the named dependency.
func (in ResourceInput) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
}

// ResourceFactory declares and builds one shared resource.
//
// A returned value implementing io.Closer is closed by
// [Result.Close] in reverse construction order. A constructor that
// returns something it does NOT want closed should wrap it.
type ResourceFactory interface {
	Spec() ResourceSpec
	New(ctx context.Context, in ResourceInput) (any, error)
}

// ItemResolver is implemented by container resources that hold named
// items — a workspace registry's workspaces, a sandbox registry's
// runners. The scalar dep form "resource/item" resolves through it.
//
// Not every resource is a container. An inference.Runtime, a tool
// Assembly or a memory instance is a single object: models and tools
// are selected inside a call, not bound per dep. Those bind whole,
// and binding them with an item name is a build error.
type ItemResolver interface {
	ResolveItem(ref string) (any, bool)
}

// HookInput is what a hook / before / after factory receives.
type HookInput struct {
	// Settings is the factory-owned subtree; decode with
	// [DecodeSettings].
	Settings *yamlv3.Node

	// Deps holds resolved dependencies keyed by the names used in the
	// document, so a hook can reach a store or a memory instance the
	// resource area built.
	Deps map[string]any
}

// Dep returns the named dependency.
func (in HookInput) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
}

// ObserverFactory builds one lifecycle hook. Factories MUST decode
// settings strictly (see [DecodeSettings]) so a typo in YAML fails
// the build rather than silently dropping policy.
type ObserverFactory func(ctx context.Context, in HookInput) (agent.Observer, error)

// PreparerFactory builds the [agent.Preparer] seed hook.
type PreparerFactory func(ctx context.Context, in HookInput) (agent.Preparer, error)

// RefereeFactory builds one [agent.Referee] decision hook.
type RefereeFactory func(ctx context.Context, in HookInput) (agent.Referee, error)

// Instance is one assembled, runnable agent: identity + engine + the
// per-call options the document declared. Execute appends the
// document-derived options before the caller's own, so a call site
// can still override policy per invocation.
type Instance struct {
	Agent  agent.Agent
	Engine agent.Engine

	opts []agent.ExecuteOption
}

// Execute runs one turn of this instance. Caller options are appended
// AFTER the document-derived ones and therefore win on conflict (the
// harness's caller-supplied-wins rule).
func (i *Instance) Execute(ctx context.Context, req agent.Request, opts ...agent.ExecuteOption) (*agent.Result, error) {
	all := make([]agent.ExecuteOption, 0, len(i.opts)+len(opts))
	all = append(all, i.opts...)
	all = append(all, opts...)
	return agent.Execute(ctx, i.Agent, i.Engine, req, all...)
}

// builtResource is the internal build-time view of one shared
// resource: the constructed value plus its complete static spec.
type builtResource struct {
	spec  ResourceSpec
	value any
}

// Result is Build's output: the assembled instances plus the built
// resource area. The result owns resource lifecycle; call Close when
// the application shuts down.
type Result struct {
	instances map[string]*Instance
	resources map[string]any
	closers   []builtCloser // construction order; closed in reverse
	closeOnce sync.Once
	closeErr  error
}

type builtCloser struct {
	name  string
	value io.Closer
}

// Close releases every built resource that implements io.Closer, in
// reverse construction order — so a resource is always closed before
// anything it depended on. Closing continues best-effort through the
// whole list; the joined errors are returned.
//
// Values bound through a source are NOT closed: the host owns them.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		var errs []error
		for i := len(r.closers) - 1; i >= 0; i-- {
			if err := r.closers[i].value.Close(); err != nil {
				errs = append(errs, fmt.Errorf("resource %q close: %w", r.closers[i].name, err))
			}
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// Instance returns the assembled instance with id.
func (r *Result) Instance(id string) (*Instance, bool) {
	if r == nil {
		return nil, false
	}
	inst, ok := r.instances[id]
	return inst, ok
}

// InstanceNames returns assembled instance IDs in stable order.
func (r *Result) InstanceNames() []string {
	if r == nil {
		return nil
	}
	return sortedKeys(r.instances)
}

// ResourceNames returns built resource names in stable order.
func (r *Result) ResourceNames() []string {
	if r == nil {
		return nil
	}
	return sortedKeys(r.resources)
}

// ResourceAs returns a named resource as T. Missing names are
// errdefs.NotFound; Go type mismatches are errdefs.Validation.
func ResourceAs[T any](r *Result, name string) (T, error) {
	var zero T
	if r == nil {
		return zero, errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	v, ok := r.resources[name]
	if !ok {
		return zero, errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	out, ok := v.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"deploy result: resource %q has Go type %T, want %v",
			name, v, reflect.TypeFor[T]())
	}
	return out, nil
}

// fail joins a build error with every cleanup error.
func (r *Result) fail(err error) error {
	return errors.Join(err, r.Close())
}

// Builder turns validated Documents into a Result. It binds:
//
//   - an [agent.Registry] of engine factories (engine.kind lookup);
//   - named resource (kind, impl) factories — how a resources
//     entry becomes a shared instance;
//   - named dep SOURCES — how a host-owned instance enters;
//   - named hook / before / after factories.
//
// No global registration: two Builders in one process stay
// independent. The Builder imports no module config package; each
// module plugs itself in as an impl, so a deployment links only the
// integrations it actually names.
type Builder struct {
	engines   *agent.Registry
	sources   map[string]SourceFunc
	resources map[resourceKey]registeredResource
	preparers map[string]PreparerFactory
	observers map[string]ObserverFactory
	referees  map[string]RefereeFactory
}

type resourceKey struct {
	kind string
	impl string
}

type registeredResource struct {
	spec    ResourceSpec
	factory ResourceFactory
}

// NewBuilder returns a Builder over the given engine registry with
// the built-in after factories registered. A nil registry panics — a
// Builder without engine kinds cannot assemble anything.
func NewBuilder(engines *agent.Registry) *Builder {
	if engines == nil {
		panic("deploy.NewBuilder: engine registry is nil")
	}
	b := &Builder{
		engines:   engines,
		sources:   make(map[string]SourceFunc),
		resources: make(map[resourceKey]registeredResource),
		preparers: make(map[string]PreparerFactory),
		observers: make(map[string]ObserverFactory),
		referees:  make(map[string]RefereeFactory),
	}
	b.registerBuiltins()
	return b
}

// RegisterResource registers factory by its (kind, impl) pair.
func (b *Builder) RegisterResource(factory ResourceFactory) error {
	if isNil(factory) {
		return errdefs.Validationf("deploy.Builder: nil resource factory")
	}
	spec := cloneResourceSpec(factory.Spec())
	if err := validateResourceSpec(spec); err != nil {
		return err
	}
	key := resourceKey{spec.Kind, spec.Impl}
	if _, dup := b.resources[key]; dup {
		return errdefs.Validationf(
			"deploy.Builder: resource kind %q impl %q already registered",
			spec.Kind, spec.Impl)
	}
	b.resources[key] = registeredResource{spec: spec, factory: factory}
	return nil
}

// MustRegisterResource is RegisterResource that panics on error.
func (b *Builder) MustRegisterResource(factory ResourceFactory) {
	if err := b.RegisterResource(factory); err != nil {
		panic(err)
	}
}

// ResourceSpecs returns defensive copies of all registered resource
// specs in stable kind-then-impl order.
func (b *Builder) ResourceSpecs() []ResourceSpec {
	out := make([]ResourceSpec, 0, len(b.resources))
	for _, registered := range b.resources {
		out = append(out, cloneResourceSpec(registered.spec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].Impl < out[j].Impl
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// RegisterSource adds (or replaces) the resolver for a dep source
// name. Empty names and nil funcs are programming bugs.
func (b *Builder) RegisterSource(name string, fn SourceFunc) {
	if name == "" {
		panic("deploy.RegisterSource: name is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterSource: source %q is nil", name))
	}
	b.sources[name] = fn
}

// RegisterObserver adds (or replaces) a lifecycle hook factory.
func (b *Builder) RegisterObserver(typ string, fn ObserverFactory) {
	if typ == "" {
		panic("deploy.RegisterHook: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterHook: factory for type %q is nil", typ))
	}
	b.observers[typ] = fn
}

// RegisterPreparer adds (or replaces) a BeforeExecute factory.
func (b *Builder) RegisterPreparer(typ string, fn PreparerFactory) {
	if typ == "" {
		panic("deploy.RegisterBefore: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterBefore: factory for type %q is nil", typ))
	}
	b.preparers[typ] = fn
}

// RegisterReferee adds (or replaces) an AfterExecute factory.
func (b *Builder) RegisterReferee(typ string, fn RefereeFactory) {
	if typ == "" {
		panic("deploy.RegisterAfter: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterAfter: factory for type %q is nil", typ))
	}
	b.referees[typ] = fn
}

// Build assembles the resource area and every agent in doc.
//
// Resources are constructed first, in dependency order, so a resource
// can bind another (a sandbox registry over a workspace registry).
// Agents follow in sorted id order. On any failure the
// already-constructed resources are closed before the error returns.
//
// A resource nothing binds is a build error unless it declares
// export: true for application retrieval through ResourceAs. "Binds"
// counts every consumer: another resource, an agent's engine dep, or
// a hook dep.
func (b *Builder) Build(ctx context.Context, doc Document) (*Result, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	order, err := resourceOrder(doc.Resources)
	if err != nil {
		return nil, err
	}

	res := &Result{
		instances: make(map[string]*Instance, len(doc.Agents)),
		resources: make(map[string]any, len(doc.Resources)),
	}
	used := make(map[string]bool, len(doc.Resources))
	built := make(map[string]builtResource, len(doc.Resources))

	for _, name := range order {
		entry := doc.Resources[name]
		registered, ok := b.resources[resourceKey{entry.Kind, entry.Impl}]
		if !ok {
			return nil, res.fail(errdefs.NotFound(fmt.Errorf(
				"deploy config resources[%q]: no constructor registered for kind %q impl %q",
				name, entry.Kind, entry.Impl)))
		}
		spec := registered.spec
		where := fmt.Sprintf("deploy config resources[%q]", name)
		deps, err := b.resolveResourceDeps(ctx, spec, entry.Deps, built, used, where)
		if err != nil {
			return nil, res.fail(err)
		}
		v, err := registered.factory.New(ctx, ResourceInput{Settings: entry.Settings.Node(), Deps: deps})
		if err != nil {
			return nil, res.fail(fmt.Errorf(
				"deploy config resources[%q] (%s/%s): %w",
				name, entry.Kind, entry.Impl, err))
		}
		if isNil(v) {
			return nil, res.fail(errdefs.Internalf(
				"deploy config resources[%q]: constructor for %s/%s returned nil",
				name, entry.Kind, entry.Impl))
		}
		built[name] = builtResource{spec: spec, value: v}
		res.resources[name] = v
		if c, ok := v.(io.Closer); ok {
			res.closers = append(res.closers, builtCloser{name, c})
		}
	}

	for _, id := range sortedKeys(doc.Agents) {
		inst, err := b.buildOne(ctx, id, doc.Agents[id], built, used)
		if err != nil {
			return nil, res.fail(err)
		}
		res.instances[id] = inst
	}

	for _, name := range order {
		if !used[name] && !doc.Resources[name].Export {
			return nil, res.fail(errdefs.Validation(fmt.Errorf(
				"deploy config resources[%q]: nothing binds this resource", name)))
		}
	}
	return res, nil
}

// resourceOrder returns resource names in construction order: every
// resource follows the resources it binds. Cycles are reported with
// the names involved, since a cycle in config is unbuildable by any
// ordering.
func resourceOrder(entries map[string]ResourceEntry) ([]string, error) {
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := make(map[string]int, len(entries))
	order := make([]string, 0, len(entries))

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case active:
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources: dependency cycle %v", append(path, name)))
		}
		state[name] = active
		entry := entries[name]
		for _, depName := range sortedKeys(entry.Deps) {
			ref := entry.Deps[depName]
			if ref.Resource == "" {
				continue // a source needs no ordering
			}
			if _, ok := entries[ref.Resource]; !ok {
				return errdefs.NotFound(fmt.Errorf(
					"deploy config resources[%q].deps[%q]: resource %q is not defined (defined: %v)",
					name, depName, ref.Resource, sortedKeys(entries)))
			}
			if err := visit(ref.Resource, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		order = append(order, name)
		return nil
	}

	for _, name := range sortedKeys(entries) {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (b *Builder) buildOne(ctx context.Context, id string, entry AgentEntry, resources map[string]builtResource, used map[string]bool) (*Instance, error) {
	factory, ok := b.engines.Lookup(entry.Engine.Kind)
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"deploy config agents[%q]: engine kind %q is not registered", id, entry.Engine.Kind))
	}

	spec := factory.Spec()
	deps, err := b.resolveDeps(ctx, id, spec, entry.Deps, resources, used)
	if err != nil {
		return nil, err
	}

	eng, err := factory.New(ctx, agent.Config{
		Deps:     deps,
		Settings: entry.Engine.Settings,
	})
	if err != nil {
		return nil, fmt.Errorf("deploy config agents[%q]: engine %q build: %w", id, entry.Engine.Kind, err)
	}

	inst := &Instance{
		Agent: agent.Agent{
			ID: id,
			Card: agent.AgentCard{
				Name:        entry.Card.Name,
				Description: entry.Card.Description,
			},
			Tools: entry.Tools,
		},
		Engine: eng,
	}

	for i, p := range entry.Prepare {
		preparer, err := b.buildPreparer(ctx, id, i, p, resources, used)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithPreparer(preparer))
	}
	for i, o := range entry.Observe {
		observer, err := b.buildObserver(ctx, id, i, o, resources, used)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithObserver(observer))
	}
	for i, r := range entry.Referees {
		referee, err := b.buildReferee(ctx, id, i, r, resources, used)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithReferee(referee))
	}
	if entry.Policy.MaxRevise > 0 {
		inst.opts = append(inst.opts, agent.WithMaxRevise(entry.Policy.MaxRevise))
	}
	if len(entry.Policy.ArtifactChannels) > 0 {
		inst.opts = append(inst.opts, agent.WithArtifactChannels(entry.Policy.ArtifactChannels...))
	}
	return inst, nil
}

// resolveDeps binds every engine dep and validates the result against
// the factory's DepSpec list: unknown dep names and missing required
// deps are build errors.
func (b *Builder) resolveDeps(ctx context.Context, id string, spec agent.EngineSpec, refs map[string]DepRef, resources map[string]builtResource, used map[string]bool) (map[string]any, error) {
	declared := make(map[string]agent.DepSpec, len(spec.Deps))
	for _, ds := range spec.Deps {
		declared[ds.Name] = ds
	}

	deps := make(map[string]any, len(refs))
	for _, name := range sortedKeys(refs) {
		ref := refs[name]
		ds, ok := declared[name]
		if !ok {
			return nil, errdefs.Validation(fmt.Errorf(
				"deploy config agents[%q].deps[%q]: engine kind %q declares no such dep (declared: %v)",
				id, name, spec.Kind, depNames(spec.Deps)))
		}
		v, err := b.resolveRef(ctx, name, ref, ds.Type, resources, used,
			fmt.Sprintf("deploy config agents[%q]", id))
		if err != nil {
			return nil, err
		}
		deps[name] = v
	}

	for _, ds := range spec.Deps {
		if ds.Required {
			if _, ok := deps[ds.Name]; !ok {
				return nil, errdefs.NotFound(fmt.Errorf(
					"deploy config agents[%q]: required dep %q (%s) is not bound", id, ds.Name, ds.Type))
			}
		}
	}
	return deps, nil
}

// resolveResourceDeps validates a resource entry against its factory
// spec and resolves each declared binding.
func (b *Builder) resolveResourceDeps(
	ctx context.Context,
	spec ResourceSpec,
	refs map[string]DepRef,
	resources map[string]builtResource,
	used map[string]bool,
	where string,
) (map[string]any, error) {
	declared := make(map[string]ResourceDepSpec, len(spec.Deps))
	for _, ds := range spec.Deps {
		declared[ds.Name] = ds
	}

	deps := make(map[string]any, len(refs))
	for _, name := range sortedKeys(refs) {
		ds, ok := declared[name]
		if !ok {
			return nil, errdefs.Validation(fmt.Errorf(
				"%s.deps[%q]: resource %s/%s declares no such dep (declared: %v)",
				where, name, spec.Kind, spec.Impl, resourceDepNames(spec.Deps)))
		}
		v, err := b.resolveRef(ctx, name, refs[name], ds.Type, resources, used, where)
		if err != nil {
			return nil, err
		}
		deps[name] = v
	}
	for _, ds := range spec.Deps {
		if ds.Required {
			if _, ok := deps[ds.Name]; !ok {
				return nil, errdefs.NotFound(fmt.Errorf(
					"%s: required dep %q (%s) is not bound",
					where, ds.Name, ds.Type))
			}
		}
	}
	return deps, nil
}

// resolveRefs binds a hook dep map, which has no static spec.
func (b *Builder) resolveRefs(ctx context.Context, refs map[string]DepRef, resources map[string]builtResource, used map[string]bool, where string) (map[string]any, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(refs))
	for _, name := range sortedKeys(refs) {
		v, err := b.resolveRef(ctx, name, refs[name], "", resources, used, where)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

// resolveRef binds one dep. wantType is the DepSpec.Type to enforce
// on a whole-resource binding, or "" where no static type is
// declared.
func (b *Builder) resolveRef(ctx context.Context, name string, ref DepRef, wantType string, resources map[string]builtResource, used map[string]bool, where string) (any, error) {
	if ref.Resource != "" {
		return resolveResourceRef(name, ref, wantType, resources, used, where)
	}
	src, ok := b.sources[ref.Source]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s.deps[%q]: source %q is not registered", where, name, ref.Source))
	}
	v, err := src(ctx, ref.Ref)
	if err != nil {
		return nil, fmt.Errorf("%s.deps[%q] (source %q, ref %q): %w",
			where, name, ref.Source, ref.Ref, err)
	}
	return v, nil
}

// resolveResourceRef binds a dep from a document resource.
//
// With Ref empty the whole resource is bound; when the consumer
// declares a type, the resource's kind must
// match it, which is what makes a resource the right category for the
// slot. With Ref set, ItemType is checked before ResolveItem is called.
func resolveResourceRef(name string, ref DepRef, wantType string, resources map[string]builtResource, used map[string]bool, where string) (any, error) {
	br, ok := resources[ref.Resource]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s.deps[%q]: resource %q is not defined (defined: %v)",
			where, name, ref.Resource, sortedKeys(resources)))
	}
	if ref.Ref == "" {
		if wantType != "" && br.spec.Kind != wantType {
			return nil, errdefs.Validation(fmt.Errorf(
				"%s.deps[%q]: resource %q has kind %q, dep expects %q",
				where, name, ref.Resource, br.spec.Kind, wantType))
		}
		used[ref.Resource] = true
		return br.value, nil
	}
	if br.spec.ItemType == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"%s.deps[%q]: resource %q (kind %q) is not a container: no item type is declared; item %q cannot be resolved",
			where, name, ref.Resource, br.spec.Kind, ref.Ref))
	}
	if wantType != "" && br.spec.ItemType != wantType {
		return nil, errdefs.Validation(fmt.Errorf(
			"%s.deps[%q]: resource %q has item type %q, dep expects %q",
			where, name, ref.Resource, br.spec.ItemType, wantType))
	}
	container, ok := br.value.(ItemResolver)
	if !ok {
		return nil, errdefs.Internalf(
			"%s.deps[%q]: resource %q factory %s/%s declares item type %q but returned %T without ItemResolver",
			where, name, ref.Resource, br.spec.Kind, br.spec.Impl,
			br.spec.ItemType, br.value)
	}
	item, ok := container.ResolveItem(ref.Ref)
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s.deps[%q]: resource %q has no item %q",
			where, name, ref.Resource, ref.Ref))
	}
	if isNil(item) {
		return nil, errdefs.Internalf(
			"%s.deps[%q]: resource %q factory %s/%s resolved item %q as nil",
			where, name, ref.Resource, br.spec.Kind, br.spec.Impl, ref.Ref)
	}
	used[ref.Resource] = true
	return item, nil
}

func (b *Builder) buildObserver(ctx context.Context, id string, idx int, h ObserverEntry, resources map[string]builtResource, used map[string]bool) (agent.Observer, error) {
	where := fmt.Sprintf("deploy config agents[%q].observe[%d]", id, idx)
	fn, ok := b.observers[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s: hook type %q is not registered", where, h.Type))
	}
	in, err := b.factoryInput(ctx, h, resources, used, where)
	if err != nil {
		return nil, err
	}
	hook, err := fn(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("%s (%q): %w", where, h.Type, err)
	}
	if hook == nil {
		return nil, errdefs.Internalf("%s: factory for %q returned nil", where, h.Type)
	}
	return hook, nil
}

func (b *Builder) buildPreparer(ctx context.Context, id string, idx int, h PreparerEntry, resources map[string]builtResource, used map[string]bool) (agent.Preparer, error) {
	where := fmt.Sprintf("deploy config agents[%q].prepare", id)
	fn, ok := b.preparers[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s: type %q is not registered", where, h.Type))
	}
	in, err := b.factoryInput(ctx, h, resources, used, where)
	if err != nil {
		return nil, err
	}
	before, err := fn(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("%s (%q): %w", where, h.Type, err)
	}
	if before == nil {
		return nil, errdefs.Internalf("%s: factory for %q returned nil", where, h.Type)
	}
	return before, nil
}

func (b *Builder) buildReferee(ctx context.Context, id string, idx int, h RefereeEntry, resources map[string]builtResource, used map[string]bool) (agent.Referee, error) {
	where := fmt.Sprintf("deploy config agents[%q].referees[%d]", id, idx)
	fn, ok := b.referees[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s: type %q is not registered", where, h.Type))
	}
	in, err := b.factoryInput(ctx, h, resources, used, where)
	if err != nil {
		return nil, err
	}
	after, err := fn(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("%s (%q): %w", where, h.Type, err)
	}
	if after == nil {
		return nil, errdefs.Internalf("%s: factory for %q returned nil", where, h.Type)
	}
	return after, nil
}

func (b *Builder) factoryInput(ctx context.Context, h PreparerEntry, resources map[string]builtResource, used map[string]bool, where string) (HookInput, error) {
	deps, err := b.resolveRefs(ctx, h.Deps, resources, used, where)
	if err != nil {
		return HookInput{}, err
	}
	return HookInput{Settings: h.Settings.Node(), Deps: deps}, nil
}

func depNames(specs []agent.DepSpec) []string {
	out := make([]string, len(specs))
	for i, ds := range specs {
		out[i] = ds.Name
	}
	return out
}

func resourceDepNames(specs []ResourceDepSpec) []string {
	out := make([]string, len(specs))
	for i, ds := range specs {
		out[i] = ds.Name
	}
	return out
}

func cloneResourceSpec(spec ResourceSpec) ResourceSpec {
	spec.Deps = append([]ResourceDepSpec(nil), spec.Deps...)
	return spec
}

func validateResourceSpec(spec ResourceSpec) error {
	if spec.Kind == "" {
		return errdefs.Validationf("deploy resource factory: kind is empty")
	}
	if spec.Impl == "" {
		return errdefs.Validationf(
			"deploy resource factory %q: impl is empty", spec.Kind)
	}
	seen := make(map[string]struct{}, len(spec.Deps))
	for i, dep := range spec.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"deploy resource factory %s/%s: deps[%d].name is empty",
				spec.Kind, spec.Impl, i)
		}
		if dep.Type == "" {
			return errdefs.Validationf(
				"deploy resource factory %s/%s: dep %q type is empty",
				spec.Kind, spec.Impl, dep.Name)
		}
		if _, dup := seen[dep.Name]; dup {
			return errdefs.Validationf(
				"deploy resource factory %s/%s: duplicate dep %q",
				spec.Kind, spec.Impl, dep.Name)
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
