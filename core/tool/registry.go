package tool

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ConflictPolicy decides what happens when two sources contribute a
// tool with the same name.
type ConflictPolicy int

const (
	// ConflictError fails construction on any duplicate name. This is
	// the default: a silent overwrite usually means two sources
	// disagree about who owns a tool.
	ConflictError ConflictPolicy = iota
	// ConflictOverwrite lets a later source (in the explicit dep
	// order) replace an earlier tool with the same name. Use it when
	// a deployment intentionally overrides a base tool.
	ConflictOverwrite
)

type options struct {
	conflict ConflictPolicy
}

// Option configures a Registry.
type Option func(*options)

// WithConflictPolicy sets the duplicate-name policy. The default is
// ConflictError; pass ConflictOverwrite to let later sources win.
func WithConflictPolicy(p ConflictPolicy) Option {
	return func(o *options) { o.conflict = p }
}

// Registry aggregates tools from many [Source] values. It implements
// [Catalog]; the underlying execution surface for an Executor is the
// same value. The tool set is fixed at construction — sources are a
// build-time concern — and changes at runtime only through the
// [Registrar] surface (Add/Remove), which deferred sources use to
// publish tools discovered after construction.
//
// A Registry is safe for concurrent use. Close releases every
// contributed tool that implements io.Closer (deferred proxies
// included), forbids further lazy loads, and detaches the sources
// that were attached at construction.
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]Tool
	order     []string
	conflict  ConflictPolicy
	closed    bool
	detachers []RegistryDetacher
	// defs caches the sorted definitions; nil means rebuild. Atomic because a
	// deferred tool invalidates it while holding its own lock.
	defs atomic.Pointer[[]message.ToolDefinition]
	// defsEpoch moves whenever the definition set may have changed, so a reader
	// that caches something derived from it (a search index) can tell whether
	// its copy is still the current one.
	defsEpoch atomic.Uint64
}

// NewRegistry builds a registry from sources in order. With the
// default ConflictError policy a duplicate name is an error; with
// ConflictOverwrite the later source wins. Sources that implement
// [RegistryAttacher] receive the new registry's [Registrar] before
// NewRegistry returns, so tools they discover after construction (a
// background MCP connect, a refresh) still reach it; sources that
// implement [RegistryDetacher] are notified when the registry closes.
func NewRegistry(sources []Source, opts ...Option) (*Registry, error) {
	o := options{conflict: ConflictError}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	r := &Registry{
		tools:    make(map[string]Tool),
		conflict: o.conflict,
	}
	for _, src := range sources {
		if src == nil {
			return nil, errdefs.Validationf("tool: nil source")
		}
		for _, t := range src.Tools() {
			if t == nil {
				return nil, errdefs.Validationf(
					"tool: source returned a nil tool")
			}
			if err := r.add(t); err != nil {
				return nil, err
			}
		}
		for _, lt := range src.LazyTools() {
			if err := lt.Validate(); err != nil {
				return nil, err
			}
			if err := r.add(newLazyProxy(r, lt)); err != nil {
				return nil, err
			}
		}
	}
	for _, src := range sources {
		attacher, ok := src.(RegistryAttacher)
		if !ok {
			continue
		}
		attacher.Attach(r)
		if detacher, ok := src.(RegistryDetacher); ok {
			r.detachers = append(r.detachers, detacher)
		}
	}
	return r, nil
}

// Add registers one tool at runtime. It follows the same duplicate
// policy as construction and fails after Close.
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return errdefs.Validationf("tool: nil tool")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errdefs.NotAvailablef("tool: registry is closed")
	}
	return r.addLocked(t)
}

// Remove unregisters the named tool at runtime. If the removed tool
// implements io.Closer, it is closed so runtime removals (e.g. a
// shrunk MCP tool projection) release whatever resources the tool
// holds; the close happens outside the registry lock and is
// best-effort. Unknown names are ignored; after Close it is a no-op.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	t, ok := r.tools[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	r.invalidateDefinitions()
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	if c, ok := t.(io.Closer); ok {
		if err := c.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "tool registry: close removed tool failed", err,
				attribute.String(telemetry.AttrToolName, name))
		}
	}
}

func (r *Registry) add(t Tool) error {
	def := t.Definition()
	if strings.TrimSpace(def.Name) == "" {
		return errdefs.Validationf("tool: tool definition name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addLocked(t)
}

func (r *Registry) addLocked(t Tool) error {
	r.invalidateDefinitions()
	def := t.Definition()
	if strings.TrimSpace(def.Name) == "" {
		return errdefs.Validationf("tool: tool definition name is required")
	}
	if _, exists := r.tools[def.Name]; exists {
		if r.conflict == ConflictError {
			return errdefs.Conflictf(
				"tool: duplicate tool %q from multiple sources", def.Name)
		}
		r.tools[def.Name] = t // later source wins, first position kept
		return nil
	}
	r.tools[def.Name] = t
	r.order = append(r.order, def.Name)
	return nil
}

var _ Registrar = (*Registry)(nil)

// Get implements Catalog. A deferred tool returns its proxy, which
// serves the placeholder definition until first Execute.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Definitions implements Catalog, sorted by name.
//
// The list is cached: every builtin tool rebuilds and re-marshals its schema in
// Definition(), and the loop asks for the whole set once per round. The cache is
// dropped whenever the set can change — Add, Remove, or a deferred tool finishing
// its load (its definition replaces the placeholder) — and the pointer is atomic
// so that a load, which holds the proxy lock, can invalidate without taking the
// registry lock and inverting the lock order.
func (r *Registry) Definitions() []message.ToolDefinition {
	if cached := r.defs.Load(); cached != nil {
		return cloneDefinitions(*cached)
	}
	r.mu.RLock()
	defs := make([]message.ToolDefinition, 0, len(r.tools))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	r.mu.RUnlock()
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	r.defs.Store(&defs)
	return cloneDefinitions(defs)
}

// cloneDefinitions returns a copy, so a caller that appends cannot corrupt the
// cache.
func cloneDefinitions(defs []message.ToolDefinition) []message.ToolDefinition {
	out := make([]message.ToolDefinition, len(defs))
	copy(out, defs)
	return out
}

// DefinitionsEpoch reports a counter that moves whenever the definition set may
// have changed: an Add, a Remove, or a deferred tool finishing its load.
//
// It is a hint for readers that cache something derived from [Definitions] — a
// BM25 index over the catalog, say — so they can tell whether their copy is
// still the current one. It is deliberately not a method on [Catalog]: a fixture
// or a wrapper that cannot answer it keeps recomputing, which is what a failed
// [tool/engine] probe falls back to. A caller still needs one Definitions call
// after the epoch changes to see the new set; the epoch only rules out reuse.
func (r *Registry) DefinitionsEpoch() uint64 { return r.defsEpoch.Load() }

// invalidateDefinitions drops the cached definitions. It is safe to call while
// holding any lock: the pointer is atomic.
//
// The data goes before the version, and the order is load bearing. A reader that
// derives something from Definitions — the BM25 index tool_search keeps — reads
// the epoch and then calls Definitions, so publishing the epoch first would let
// it observe the new version together with the pre-mutation cached slice and
// then cache a stale derivation under the current epoch, permanently. Clearing
// first makes "observed epoch E+1" imply "the cache is already empty"; the
// opposite interleaving costs one redundant rebuild at the old epoch.
func (r *Registry) invalidateDefinitions() {
	r.defs.Store(nil)
	r.defsEpoch.Add(1)
}

// Names returns the registered tool names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	out = append(out, r.order...)
	sort.Strings(out)
	return out
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Close releases every tool that implements io.Closer (in reverse
// registration order), marks the registry closed so deferred tools can
// no longer load, and notifies every attached [RegistryDetacher]. It
// is idempotent.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	order := append([]string(nil), r.order...)
	detachers := r.detachers
	r.detachers = nil
	r.mu.Unlock()

	var first error
	for _, o := range slices.Backward(order) {
		t, _ := r.Get(o)
		if c, ok := t.(io.Closer); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	for _, detacher := range detachers {
		detacher.Detach(r)
	}
	return first
}

// lazyProxy is the deferred tool stored in a Registry. It serves the
// placeholder definition, loads the real tool once on first Execute
// (serialised, so concurrent callers share one load), and refuses to
// load after the registry closes.
type lazyProxy struct {
	registry *Registry
	spec     LazyTool

	mu     sync.Mutex
	inner  Tool
	err    error
	loaded bool
	closed bool
}

func newLazyProxy(registry *Registry, spec LazyTool) *lazyProxy {
	return &lazyProxy{registry: registry, spec: spec}
}

func (p *lazyProxy) Definition() message.ToolDefinition {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded && p.inner != nil {
		return p.inner.Definition()
	}
	return p.spec.Placeholder
}

func (p *lazyProxy) Execute(ctx context.Context, arguments string) (message.Content, error) {
	t, err := p.load(ctx)
	if err != nil {
		return message.Content{}, err
	}
	return t.Execute(ctx, arguments)
}

// EnsureLoaded forces the deferred load without executing the tool.
// It is how tool_search makes a discovered tool's real definition
// available to the next round.
func (p *lazyProxy) EnsureLoaded(ctx context.Context) error {
	_, err := p.load(ctx)
	return err
}

// Metadata forwards the loaded tool's metadata, or a conservative zero
// while unloaded.
func (p *lazyProxy) Metadata() ToolMeta {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded && p.inner != nil {
		return MetadataOf(p.inner)
	}
	return ToolMeta{}
}

func (p *lazyProxy) load(ctx context.Context) (Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errdefs.NotAvailablef(
			"tool %q: registry is closed", p.spec.Name)
	}
	if p.loaded {
		return p.inner, p.err
	}
	p.inner, p.err = p.loadLazy(ctx)
	p.loaded = true
	// The real definition replaces the placeholder: the cached set is stale.
	if p.registry != nil {
		p.registry.invalidateDefinitions()
	}
	if p.err != nil {
		telemetry.WarnErr(ctx, "tool registry: lazy tool load failed", p.err,
			attribute.String(telemetry.AttrToolName, p.spec.Name))
	}
	return p.inner, p.err
}

// loadLazy materialises a deferred tool under its own span.
//
// It is the miss path only: a cache hit returns above this function, so the span
// measures real work — an MCP handshake, a process spawn, a schema fetch —
// instead of adding a span to every executed call.
func (p *lazyProxy) loadLazy(ctx context.Context) (Tool, error) {
	ctx, span := telemetry.TracerWithSuffix("tool").Start(ctx, "tool.load",
		trace.WithAttributes(attribute.String(telemetry.AttrToolName, p.spec.Name)))
	// The runtime identity the host attached, so a lazy load is attributable to
	// the runtime that triggered it.
	span.SetAttributes(runctx.Attrs(ctx)...)
	defer span.End()

	start := time.Now()
	inner, err := p.spec.Load(ctx)
	duration := time.Since(start)

	status := "success"
	if err != nil {
		status = "error"
		// The load error is local (a file, a process, a handshake); the
		// message goes to the warn log below, not onto the span.
		span.SetStatus(codes.Error, "load_failed")
	}
	recordLoadMetrics(ctx, p.spec.Name, status, duration)
	return inner, err
}

// recordLoadMetrics emits the deferred-load series.
func recordLoadMetrics(ctx context.Context, name, status string, duration time.Duration) {
	meter := telemetry.MeterWithSuffix("tool")
	nameAttr := metric.WithAttributes(attribute.String(telemetry.AttrToolName, name))
	if counter, err := meter.Int64Counter("loads.total",
		metric.WithDescription("Deferred tool loads by outcome")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrToolName, name),
			attribute.String("status", status)))
	}
	if histogram, err := meter.Float64Histogram("load.duration.seconds",
		metric.WithDescription("Deferred tool load duration")); err == nil {
		histogram.Record(ctx, duration.Seconds(), nameAttr)
	}
}

func (p *lazyProxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.loaded {
		if c, ok := p.inner.(io.Closer); ok {
			return c.Close()
		}
	}
	return nil
}

var (
	_ Catalog   = (*Registry)(nil)
	_ Tool      = (*lazyProxy)(nil)
	_ io.Closer = (*lazyProxy)(nil)
)
