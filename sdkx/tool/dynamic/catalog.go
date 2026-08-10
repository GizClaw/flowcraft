package dynamic

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

// Catalog is the session-scoped, model-facing view over a shared
// tool.Registry. It implements the tool.Catalog contract with
// Exposure-based injection:
//
//   - Get and Definitions never load, never block on the network, and
//     never fail — every deferred source is loaded through Load /
//     EnsureLoaded / Execute instead;
//   - Definitions is a pure function of the current state, so repeated
//     calls with the same state return identical, sorted output;
//   - the underlying Registry remains the execution surface, so
//     injection never gates callability.
//
// A Catalog is per-session state: create it when a conversation starts,
// advance turns after each inference round, and Close it when the
// conversation ends to release deferred tool sessions.
type Catalog struct {
	reg *sdktool.Registry

	mu      sync.RWMutex
	policy  Policy
	st      state
	proxies map[string]*LazyTool
	closed  bool
}

// Option configures the catalog policy. Options mutate the policy in
// order, so WithPolicy may be overridden by later options.
type Option func(*Policy)

// WithPolicy replaces the whole policy.
func WithPolicy(p Policy) Option {
	return func(dst *Policy) { *dst = p }
}

// WithDefaultExposure sets the exposure applied to tools without an
// explicit entry.
func WithDefaultExposure(e Exposure) Option {
	return func(p *Policy) { p.Default = e }
}

// WithExposure pins one tool to an explicit exposure.
func WithExposure(name string, e Exposure) Option {
	return func(p *Policy) { p.Exposures[name] = e }
}

// WithSelectedRetention sets how many rounds a selected tool stays
// visible (M).
func WithSelectedRetention(rounds int) Option {
	return func(p *Policy) { p.SelectedRetention = rounds }
}

// WithRecentWindow sets how many rounds a used direct tool stays
// visible.
func WithRecentWindow(rounds int) Option {
	return func(p *Policy) { p.RecentWindow = rounds }
}

// WithBudget sets the per-turn definition budget.
func WithBudget(b Budget) Option {
	return func(p *Policy) { p.Budget = b }
}

// New creates a catalog over reg with the given policy options. A nil
// registry or invalid policy is a programming bug and panics.
func New(reg *sdktool.Registry, opts ...Option) *Catalog {
	if reg == nil {
		panic("dynamic.New: registry is nil")
	}
	policy := DefaultPolicy()
	for _, opt := range opts {
		if opt != nil {
			opt(&policy)
		}
	}
	if err := policy.Validate(); err != nil {
		panic("dynamic.New: " + err.Error())
	}
	return &Catalog{
		reg:     reg,
		policy:  policy,
		st:      *newState(),
		proxies: make(map[string]*LazyTool),
	}
}

// Get implements tool.Catalog. It never loads: the proxy returned for a
// deferred tool stays valid and loads on Execute.
func (c *Catalog) Get(name string) (sdktool.Tool, bool) {
	return c.reg.Get(name)
}

// Definitions implements tool.Catalog: the visibility-filtered,
// budget-pruned, name-sorted definitions for this round.
func (c *Catalog) Definitions() []message.Definition {
	c.mu.RLock()
	policy := c.policyCopy()
	st := c.st.snapshot()
	c.mu.RUnlock()

	all := c.reg.Definitions()
	cands := make([]candidate, 0, len(all))
	for _, def := range all {
		cands = append(cands, candidate{
			name: def.Name,
			def:  def,
			exp:  policy.exposureOf(def.Name),
		})
	}
	visible := visibleCandidates(cands, st, policy)
	out := make([]message.Definition, 0, len(visible))
	for _, cand := range visible {
		out = append(out, cand.def)
	}
	return out
}

// Require adds names to the RequiredByName set. Tools named in a
// node's explicit tools list should be required before each round.
func (c *Catalog) Require(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.st.require(names...)
}

// Select marks names as selected for the policy's retention rounds.
// Selection carries an implicit load contract: a selected tool must be
// loaded before the next Definitions call, otherwise the model would
// see its placeholder schema. tool_search enforces this by loading each
// name before selecting; direct callers (hosts) are responsible for
// preloading via EnsureLoaded.
func (c *Catalog) Select(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.st.selectNames(names, c.policy.selectedRetention())
}

// RecordCall records that the model called name this round, refreshing
// its Selected and UsedRecently state.
func (c *Catalog) RecordCall(call message.Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.st.recordCall(call.Name, c.policy.selectedRetention())
}

// AdvanceTurn moves to the next round, expiring Selected and
// UsedRecently entries. Call it once per inference round (e.g. in
// OnTurnFinished).
func (c *Catalog) AdvanceTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.st.advanceTurn(c.policy.recentWindow())
}

// SetExposure updates one tool's exposure at runtime. It takes effect
// on the next Definitions call.
func (c *Catalog) SetExposure(name string, e Exposure) error {
	if !e.Valid() {
		return errdefs.Validationf("dynamic: exposure %q is invalid", e)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errdefs.NotAvailablef("dynamic: catalog is closed")
	}
	c.policy.Exposures[name] = e
	return nil
}

// Register adds an already-implemented tool to the shared registry and
// records its exposure. It is a convenience for hosts that build the
// dynamic surface programmatically.
func (c *Catalog) Register(t sdktool.Tool, exp Exposure) error {
	if t == nil {
		return errdefs.Validationf("dynamic: Register: tool is nil")
	}
	def := t.Definition()
	if def.Name == "" {
		return errdefs.Validationf("dynamic: Register: tool definition name is required")
	}
	if !exp.Valid() {
		return errdefs.Validationf("dynamic: exposure %q is invalid", exp)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errdefs.NotAvailablef("dynamic: catalog is closed")
	}
	c.policy.Exposures[def.Name] = exp
	c.mu.Unlock()
	c.reg.Register(t)
	return nil
}

// RegisterProxy creates a deferred proxy, registers it into the shared
// registry, and records its exposure. The loader runs only when
// EnsureLoaded / Load / Execute demands it.
func (c *Catalog) RegisterProxy(name string, loader Loader, exp Exposure, opts ...LazyOption) error {
	if name == "" {
		return errdefs.Validationf("dynamic: RegisterProxy: name is empty")
	}
	if loader == nil {
		return errdefs.Validationf("dynamic: RegisterProxy: loader is nil")
	}
	if !exp.Valid() {
		return errdefs.Validationf("dynamic: exposure %q is invalid", exp)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errdefs.NotAvailablef("dynamic: catalog is closed")
	}
	if _, exists := c.proxies[name]; exists {
		c.mu.Unlock()
		return errdefs.Conflictf("dynamic: proxy %q already registered", name)
	}
	proxy := NewLazyTool(c.reg, name, loader, opts...)
	c.proxies[name] = proxy
	c.policy.Exposures[name] = exp
	c.mu.Unlock()
	c.reg.Register(proxy)
	return nil
}

// Load eagerly loads every deferred proxy. One failing source does not
// stop the others; errors are joined. tool_search calls this before
// ranking so freshly attached servers contribute results.
func (c *Catalog) Load(ctx context.Context) error {
	c.mu.RLock()
	proxies := make([]*LazyTool, 0, len(c.proxies))
	for _, proxy := range c.proxies {
		proxies = append(proxies, proxy)
	}
	c.mu.RUnlock()

	var errs []error
	for _, proxy := range proxies {
		if err := proxy.EnsureLoaded(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EnsureLoaded loads the named proxies. With no names it behaves like
// Load.
func (c *Catalog) EnsureLoaded(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return c.Load(ctx)
	}
	var errs []error
	for _, name := range names {
		c.mu.RLock()
		proxy := c.proxies[name]
		c.mu.RUnlock()
		if proxy != nil {
			if err := proxy.EnsureLoaded(ctx); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if t, ok := c.reg.Get(name); ok {
			if lazy, ok := t.(interface{ EnsureLoaded(context.Context) error }); ok {
				if err := lazy.EnsureLoaded(ctx); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			// A concrete tool needs no loading; it is loaded by
			// definition, so selection is safe.
			continue
		}
		errs = append(errs, errdefs.NotFoundf(
			"dynamic: no deferred loader for tool %q", name))
	}
	return errors.Join(errs...)
}

// Close releases every deferred proxy session. Idempotent.
func (c *Catalog) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	proxies := make([]*LazyTool, 0, len(c.proxies))
	for _, proxy := range c.proxies {
		proxies = append(proxies, proxy)
	}
	c.proxies = make(map[string]*LazyTool)
	c.mu.Unlock()

	var errs []error
	for _, proxy := range proxies {
		if err := proxy.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Catalog) policyCopy() Policy {
	p := c.policy
	p.Exposures = make(map[string]Exposure, len(c.policy.Exposures))
	for name, e := range c.policy.Exposures {
		p.Exposures[name] = e
	}
	return p
}
