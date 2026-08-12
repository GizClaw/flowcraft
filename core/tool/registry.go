package tool

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
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
// same value. A Registry is immutable after construction — sources
// are a build-time concern, not a runtime one.
//
// A Registry is safe for concurrent use. Close releases every
// contributed tool that implements io.Closer (deferred proxies
// included) and forbids further lazy loads.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	order    []string
	conflict ConflictPolicy
	closed   bool
}

// NewRegistry builds a registry from sources in order. With the
// default ConflictError policy a duplicate name is an error; with
// ConflictOverwrite the later source wins.
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
			if err := r.add(newLazyProxy(lt)); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

func (r *Registry) add(t Tool) error {
	def := t.Definition()
	if strings.TrimSpace(def.Name) == "" {
		return errdefs.Validationf("tool: tool definition name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Get implements Catalog. A deferred tool returns its proxy, which
// serves the placeholder definition until first Execute.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Definitions implements Catalog, sorted by name.
func (r *Registry) Definitions() []message.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]message.ToolDefinition, 0, len(r.tools))
	for _, name := range r.order {
		out = append(out, r.tools[name].Definition())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
// registration order) and marks the registry closed so deferred tools
// can no longer load. It is idempotent.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	order := append([]string(nil), r.order...)
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
	return first
}

// lazyProxy is the deferred tool stored in a Registry. It serves the
// placeholder definition, loads the real tool once on first Execute
// (serialised, so concurrent callers share one load), and refuses to
// load after the registry closes.
type lazyProxy struct {
	spec LazyTool

	mu     sync.Mutex
	inner  Tool
	err    error
	loaded bool
	closed bool
}

func newLazyProxy(spec LazyTool) *lazyProxy {
	return &lazyProxy{spec: spec}
}

func (p *lazyProxy) Definition() message.ToolDefinition {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded && p.inner != nil {
		return p.inner.Definition()
	}
	return p.spec.Placeholder
}

func (p *lazyProxy) Execute(ctx context.Context, arguments string) (string, error) {
	t, err := p.load(ctx)
	if err != nil {
		return "", err
	}
	return t.Execute(ctx, arguments)
}

// EnsureLoaded forces the deferred load without executing the tool.
// It is how tool_search makes a selected tool's real definition
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
	p.inner, p.err = p.spec.Load(ctx)
	p.loaded = true
	return p.inner, p.err
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
