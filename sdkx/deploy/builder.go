package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

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
	spec  sdkconfig.ResourceSpec
	value any
}

// Result is Build's output: the assembled instances plus the built
// resource area. The result owns resource lifecycle; call Close when
// the application shuts down.
type Result struct {
	instances    map[string]*Instance
	resources    map[string]any
	closers      []*builtCloser // construction order; closed in reverse
	closerByName map[string]*builtCloser
	dependents   map[string][]string

	lifecycleMu sync.Mutex
	closingAll  bool
	closeDone   chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

type resourceCloseState uint8

const (
	resourceOpen resourceCloseState = iota
	resourceClosing
	resourceClosed
)

type builtCloser struct {
	name  string
	value io.Closer
	once  sync.Once
	mu    sync.Mutex
	err   error
	state resourceCloseState // guarded by Result.lifecycleMu
}

func (c *builtCloser) close() error {
	c.once.Do(func() {
		err := c.value.Close()
		if err != nil {
			err = fmt.Errorf("resource %q close: %w", c.name, err)
		}
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
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
		r.lifecycleMu.Lock()
		r.closingAll = true
		r.closeDone = make(chan struct{})
		r.lifecycleMu.Unlock()

		var errs []error
		for _, v := range slices.Backward(r.closers) {
			if err := r.closeBuilt(v); err != nil {
				errs = append(errs, err)
			}
		}
		r.closeErr = errors.Join(errs...)

		r.lifecycleMu.Lock()
		close(r.closeDone)
		r.lifecycleMu.Unlock()
	})
	return r.closeErr
}

// CloseResource releases one named resource owned by this Result when
// it implements io.Closer. Closing is concurrency-safe and idempotent
// across CloseResource and [Result.Close]; every caller receives the
// resource's cached close error. A resource that is not an io.Closer
// is a successful no-op. Missing names are errdefs.NotFound. If an
// unclosed direct or transitive resource still depends on name,
// CloseResource returns errdefs.Conflict without closing it.
//
// Values bound through a source are borrowed and cannot be closed
// through Result because they are not entries in its resource area.
func (r *Result) CloseResource(name string) error {
	if r == nil {
		return errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	if _, ok := r.resources[name]; !ok {
		return errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	closer, ok := r.closerByName[name]
	if !ok {
		return nil
	}

	r.lifecycleMu.Lock()
	if r.closingAll {
		done := r.closeDone
		r.lifecycleMu.Unlock()
		<-done
		return closer.close()
	}
	switch closer.state {
	case resourceClosing, resourceClosed:
		r.lifecycleMu.Unlock()
		return closer.close()
	}
	active := r.activeDependents(name)
	if len(active) > 0 {
		r.lifecycleMu.Unlock()
		return errdefs.Conflictf(
			"deploy result: resource %q is still required by active resources %v",
			name, active)
	}
	closer.state = resourceClosing
	r.lifecycleMu.Unlock()

	err := closer.close()
	r.lifecycleMu.Lock()
	closer.state = resourceClosed
	r.lifecycleMu.Unlock()
	return err
}

// activeDependents returns every direct or transitive dependent that
// has not completed closing. r.lifecycleMu must be held. Non-closers
// remain active for the Result's lifetime and therefore always block
// individual dependency closure; Result.Close bypasses this check.
func (r *Result) activeDependents(name string) []string {
	var active []string
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(dependency string) {
		for _, dependent := range r.dependents[dependency] {
			if visited[dependent] {
				continue
			}
			visited[dependent] = true
			closer, ok := r.closerByName[dependent]
			if !ok || closer.state != resourceClosed {
				active = append(active, dependent)
			}
			visit(dependent)
		}
	}
	visit(name)
	sort.Strings(active)
	return active
}

func (r *Result) closeBuilt(closer *builtCloser) error {
	r.lifecycleMu.Lock()
	if closer.state == resourceClosed {
		r.lifecycleMu.Unlock()
		return closer.close()
	}
	closer.state = resourceClosing
	r.lifecycleMu.Unlock()

	err := closer.close()
	r.lifecycleMu.Lock()
	closer.state = resourceClosed
	r.lifecycleMu.Unlock()
	return err
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

// Resource returns a borrowed reference to a named resource. The
// [Result] retains lifecycle ownership; callers must not close the
// returned value. Missing names are errdefs.NotFound, while a nil or
// typed-nil stored value is errdefs.Internal.
func (r *Result) Resource(name string) (any, error) {
	if r == nil {
		return nil, errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	v, ok := r.resources[name]
	if !ok {
		return nil, errdefs.NotFoundf("deploy result: resource %q is not found", name)
	}
	if isNil(v) {
		return nil, errdefs.Internalf(
			"deploy result: resource %q has a nil value", name)
	}
	return v, nil
}

// ResourceAs returns a named resource as T. Missing names are
// errdefs.NotFound; nil values are errdefs.Internal; Go type
// mismatches are errdefs.Validation. The returned value is borrowed:
// Result retains lifecycle ownership.
func ResourceAs[T any](r *Result, name string) (T, error) {
	var zero T
	v, err := r.Resource(name)
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"deploy result: resource %q has Go type %T, want %v",
			name, v, reflect.TypeFor[T]())
	}
	return out, nil
}

type buildConfig struct {
	externalResourceConsumers map[string]struct{}
}

// BuildOption configures one [Builder.Build] call.
type BuildOption func(*buildConfig)

// WithExternalResourceConsumers marks named deployment resources as
// borrowed by a consumer outside deploy. Such resources count as used
// without requiring export: true. Build rejects unknown names before
// invoking any resource factory.
func WithExternalResourceConsumers(names ...string) BuildOption {
	return func(cfg *buildConfig) {
		if cfg.externalResourceConsumers == nil {
			cfg.externalResourceConsumers = make(map[string]struct{}, len(names))
		}
		for _, name := range names {
			cfg.externalResourceConsumers[name] = struct{}{}
		}
	}
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
//   - named hook factories.
//
// No global registration: two Builders in one process stay
// independent. The Builder imports no module config package; each
// module plugs itself in as an impl, so a deployment links only the
// integrations it actually names.
type Builder struct {
	engines    *agent.Registry
	baseDir    string
	sources    map[string]sdkconfig.SourceFunc
	resources  map[resourceKey]registeredResource
	preparers  map[string]sdkconfig.PreparerFactory
	observers  map[string]sdkconfig.ObserverFactory
	referees   map[string]sdkconfig.RefereeFactory
	committers map[string]sdkconfig.CommitterFactory
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithBaseDir sets the directory used to resolve relative agent file
// paths. Absolute paths are used unchanged.
func WithBaseDir(dir string) BuilderOption {
	return func(b *Builder) {
		b.baseDir = dir
	}
}

type resourceKey struct {
	kind string
	impl string
}

type registeredResource struct {
	spec    sdkconfig.ResourceSpec
	factory sdkconfig.ResourceFactory
}

// NewBuilder returns a Builder over the given engine registry with
// the built-in referee factories registered. Options may configure
// loading behavior such as [WithBaseDir]. A nil registry panics — a
// Builder without engine kinds cannot assemble anything.
func NewBuilder(engines *agent.Registry, opts ...BuilderOption) *Builder {
	if engines == nil {
		panic("deploy.NewBuilder: engine registry is nil")
	}
	b := &Builder{
		engines:    engines,
		sources:    make(map[string]sdkconfig.SourceFunc),
		resources:  make(map[resourceKey]registeredResource),
		preparers:  make(map[string]sdkconfig.PreparerFactory),
		observers:  make(map[string]sdkconfig.ObserverFactory),
		referees:   make(map[string]sdkconfig.RefereeFactory),
		committers: make(map[string]sdkconfig.CommitterFactory),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	b.registerBuiltins()
	return b
}

// RegisterResource registers factory by its (kind, impl) pair.
func (b *Builder) RegisterResource(factory sdkconfig.ResourceFactory) error {
	if isNil(factory) {
		return errdefs.Validationf("deploy.Builder: nil resource factory")
	}
	spec := factory.Spec().Clone()
	if err := spec.Validate(); err != nil {
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
func (b *Builder) MustRegisterResource(factory sdkconfig.ResourceFactory) {
	if err := b.RegisterResource(factory); err != nil {
		panic(err)
	}
}

// ResourceSpecs returns defensive copies of all registered resource
// specs in stable kind-then-impl order.
func (b *Builder) ResourceSpecs() []sdkconfig.ResourceSpec {
	out := make([]sdkconfig.ResourceSpec, 0, len(b.resources))
	for _, registered := range b.resources {
		out = append(out, registered.spec.Clone())
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
func (b *Builder) RegisterSource(name string, fn sdkconfig.SourceFunc) {
	if name == "" {
		panic("deploy.RegisterSource: name is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterSource: source %q is nil", name))
	}
	b.sources[name] = fn
}

// RegisterObserver adds (or replaces) a lifecycle hook factory.
func (b *Builder) RegisterObserver(typ string, fn sdkconfig.ObserverFactory) {
	if typ == "" {
		panic("deploy.RegisterObserver: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterObserver: factory for type %q is nil", typ))
	}
	b.observers[typ] = fn
}

// RegisterPreparer adds (or replaces) a seed hook factory.
func (b *Builder) RegisterPreparer(typ string, fn sdkconfig.PreparerFactory) {
	if typ == "" {
		panic("deploy.RegisterPreparer: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterPreparer: factory for type %q is nil", typ))
	}
	b.preparers[typ] = fn
}

// RegisterReferee adds (or replaces) a decision hook factory.
func (b *Builder) RegisterReferee(typ string, fn sdkconfig.RefereeFactory) {
	if typ == "" {
		panic("deploy.RegisterReferee: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterReferee: factory for type %q is nil", typ))
	}
	b.referees[typ] = fn
}

// RegisterCommitter adds (or replaces) a durable finalizer factory.
func (b *Builder) RegisterCommitter(typ string, fn sdkconfig.CommitterFactory) {
	if typ == "" {
		panic("deploy.RegisterCommitter: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("deploy.RegisterCommitter: factory for type %q is nil", typ))
	}
	b.committers[typ] = fn
}

// Build assembles the resource area and every agent in doc.
//
// Resources are constructed first, in dependency order, so a resource
// can bind another (a sandbox registry over a workspace registry).
// Agents follow in sorted id order. On any failure the
// already-constructed resources are closed before the error returns.
//
// A resource nothing binds is a build error unless it declares
// export: true for application retrieval through ResourceAs or a
// [BuildOption] marks it as consumed externally. "Binds" counts every
// consumer: another resource, an agent's engine dep, or a hook dep.
func (b *Builder) Build(ctx context.Context, doc Document, opts ...BuildOption) (*Result, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	cfg := buildConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	for _, name := range sortedKeys(cfg.externalResourceConsumers) {
		if _, ok := doc.Resources[name]; !ok {
			return nil, errdefs.NotFoundf(
				"deploy config external resource consumer %q: resource is not defined (defined: %v)",
				name, sortedKeys(doc.Resources))
		}
	}
	agents, err := b.loadAgents(doc.Agents)
	if err != nil {
		return nil, err
	}

	order, err := resourceOrder(doc.Resources)
	if err != nil {
		return nil, err
	}

	res := &Result{
		instances:    make(map[string]*Instance, len(agents)),
		resources:    make(map[string]any, len(doc.Resources)),
		closerByName: make(map[string]*builtCloser, len(doc.Resources)),
		dependents:   make(map[string][]string, len(doc.Resources)),
	}
	for _, dependent := range order {
		seen := make(map[string]bool)
		for _, depName := range sortedKeys(doc.Resources[dependent].Deps) {
			dependency := doc.Resources[dependent].Deps[depName].Resource
			if dependency == "" || seen[dependency] {
				continue
			}
			seen[dependency] = true
			res.dependents[dependency] = append(res.dependents[dependency], dependent)
		}
	}
	used := make(map[string]bool, len(doc.Resources))
	for name := range cfg.externalResourceConsumers {
		used[name] = true
	}
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
		v, err := registered.factory.New(ctx, sdkconfig.Input{
			Settings: entry.Settings,
			Deps:     deps,
		})
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
			closer := &builtCloser{name: name, value: c}
			res.closers = append(res.closers, closer)
			res.closerByName[name] = closer
		}
	}

	for _, id := range sortedKeys(agents) {
		inst, err := b.buildOne(ctx, id, agents[id], built, used)
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

func (b *Builder) loadAgents(entries map[string]AgentEntry) (map[string]AgentEntry, error) {
	agents := make(map[string]AgentEntry, len(entries))
	for _, id := range sortedKeys(entries) {
		source := entries[id]
		if !source.usesFile() {
			agents[id] = source
			continue
		}

		path := source.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(b.baseDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			wrapped := fmt.Errorf(
				"deploy config agents[%q] file %q: read: %w", id, path, err)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errdefs.NotFound(wrapped)
			}
			return nil, errdefs.Validation(wrapped)
		}

		where := fmt.Sprintf("deploy config agents[%q] file %q", id, path)
		entry, err := parseAgentFile(data)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("%s: %w", where, err))
		}
		if err := validateAgentEntry(entry, where); err != nil {
			return nil, err
		}
		agents[id] = entry
	}
	return agents, nil
}

func parseAgentFile(data []byte) (AgentEntry, error) {
	var probe map[string]json.RawMessage
	var err error
	probe, err = utils.Decode[map[string]json.RawMessage](data)
	if err != nil {
		return AgentEntry{}, fmt.Errorf("decode agent document: %w", err)
	}
	versionRaw, ok := probe["version"]
	if !ok {
		return AgentEntry{}, fmt.Errorf("agent config version is required")
	}
	var version string
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return AgentEntry{}, fmt.Errorf("decode agent document version: %w", err)
	}
	if version != VersionV1 {
		return AgentEntry{}, fmt.Errorf(
			"agent config version %q is not supported (want %q)",
			version, VersionV1)
	}
	if _, ok := probe["file"]; ok {
		return AgentEntry{}, fmt.Errorf(
			"decode agent document: field file is not allowed in an agent file")
	}
	delete(probe, "version")
	body, err := json.Marshal(probe)
	if err != nil {
		return AgentEntry{}, fmt.Errorf("re-encode agent document: %w", err)
	}
	wire, err := sdkconfig.DecodeSettings[agentEntryWire](body)
	if err != nil {
		return AgentEntry{}, fmt.Errorf("decode agent document: %w", err)
	}
	entry := AgentEntry(wire)
	entry.source = agentEntrySourceInline
	return entry, nil
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
	for i, c := range entry.Commit {
		committer, err := b.buildCommitter(ctx, id, i, c, resources, used)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithCommitter(committer))
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
	spec sdkconfig.ResourceSpec,
	refs map[string]DepRef,
	resources map[string]builtResource,
	used map[string]bool,
	where string,
) (map[string]any, error) {
	declared := make(map[string]sdkconfig.ResourceDepSpec, len(spec.Deps))
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
	container, ok := br.value.(sdkconfig.ItemResolver)
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

func (b *Builder) buildCommitter(ctx context.Context, id string, idx int, h CommitterEntry, resources map[string]builtResource, used map[string]bool) (agent.Committer, error) {
	where := fmt.Sprintf("deploy config agents[%q].commit[%d]", id, idx)
	fn, ok := b.committers[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"%s: type %q is not registered", where, h.Type))
	}
	in, err := b.factoryInput(ctx, h, resources, used, where)
	if err != nil {
		return nil, err
	}
	committer, err := fn(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("%s (%q): %w", where, h.Type, err)
	}
	if isNil(committer) {
		return nil, errdefs.Internalf("%s: factory for %q returned nil", where, h.Type)
	}
	return committer, nil
}

func (b *Builder) factoryInput(ctx context.Context, h PreparerEntry, resources map[string]builtResource, used map[string]bool, where string) (sdkconfig.Input, error) {
	deps, err := b.resolveRefs(ctx, h.Deps, resources, used, where)
	if err != nil {
		return sdkconfig.Input{}, err
	}
	return sdkconfig.Input{Settings: h.Settings, Deps: deps}, nil
}

func depNames(specs []agent.DepSpec) []string {
	out := make([]string, len(specs))
	for i, ds := range specs {
		out[i] = ds.Name
	}
	return out
}

func resourceDepNames(specs []sdkconfig.ResourceDepSpec) []string {
	out := make([]string, len(specs))
	for i, ds := range specs {
		out[i] = ds.Name
	}
	return out
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
