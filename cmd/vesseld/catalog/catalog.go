package catalog

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// Limiter is the rate-limiting contract engine factories use to gate
// LLM calls. The fleet's per-LLMProfile token bucket implements this
// interface; we declare it here (rather than depending on fleet)
// to keep the catalog → fleet edge of the import graph one-way.
//
// Acquire MUST honour ctx — returning ctx.Err when the caller's
// context fires before a slot is available — and SHOULD return an
// errdefs-classified error so engines can react to "rate-limited
// ⇒ retry later" without parsing strings. tokens=0 means "request
// only" accounting; a positive value lets implementations enforce
// a tokens-per-window budget alongside the requests-per-window cap.
type Limiter interface {
	Acquire(ctx context.Context, vesselID string, tokens int) error
}

// Deps is the dependency bundle every factory receives. The
// resolver constructs this once per vessel build and passes it to
// each factory invocation; factories use what they need and
// ignore the rest.
//
// Centralising the bundle here (rather than letting each factory
// take a custom interface) means new shared dependencies can be
// added with one Catalog change instead of every plugin needing
// a recompile.
type Deps struct {
	// VesselID identifies the Vessel currently being assembled.
	// Factories use it for telemetry attributes and for naming
	// derived resources (e.g. tool registry sub-scopes).
	VesselID string

	// AgentName is set when the factory is being invoked for an
	// agent-specific resource (engine factory). Empty when the
	// factory is shared across agents (LLM provider, probe).
	AgentName string

	// AgentTools is the resolved tool allow-list for the agent
	// the engine is being built for. Empty / nil means "no tools
	// permitted" — this is a STRICT allow-list, not an opt-in
	// permissive default. Engine factories MUST filter the
	// ToolRegistry.Definitions() they expose to the LLM through
	// this list AND reject tool calls whose name is absent from
	// it at execution time. Set by the resolver from
	// spec.Agent.Tools (with kanban auto-injection already
	// applied by the vessel runtime when the agent is a
	// Dispatcher).
	//
	// Deprecated: this field is the legacy "build-time closure"
	// transport for the policy gate. The canonical SDK path is
	// engine.Run.Deps[depname.ToolAllowedNames] (populated by
	// agent.Run from agent.Agent.Tools — see contract-audit
	// Epic A + D). The vessel inline engine still falls back to
	// this field when the engine is invoked outside agent.Run
	// (custom drivers, legacy tests), but new engine factories
	// MUST resolve the allow-list from engine.Run.Deps via
	// engine.GetDep[[]string](run.Deps, depname.ToolAllowedNames).
	//
	// Scheduled for removal in v0.5.0 once all in-tree call sites
	// drive vessel through agent.Run. The field will then become
	// fully redundant with the run-deps path.
	AgentTools []string

	// Bus is the vessel's event bus. Factories MAY publish onto
	// it; the bus is shared with the rest of the vessel runtime.
	Bus event.Bus

	// History is the vessel's shared history (nil if disabled).
	// Engine factories typically pass it through to the runtime;
	// other factory categories rarely need it.
	History history.History

	// ToolRegistry is the daemon-shared tool registry. Engine
	// factories look up tools by id; ToolPack factories register
	// new tools here. The registry is shared across all vessels
	// in the daemon — kanban auto-injection adds per-Dispatcher
	// scoped tool ids without colliding because the registry
	// supports scope-aware lookup.
	ToolRegistry *tool.Registry

	// LLMClients is the daemon-shared map of fully-constructed
	// llm.LLM instances keyed by LLMProfile.metadata.name. The
	// resolver pre-builds one client per LLMProfile so engine
	// factories never need to deal with provider/profile/model
	// string juggling — they look up by profile name and call
	// Generate / GenerateStream directly.
	//
	// Sharing a single client per profile means cross-vessel
	// connection pools and rate-limit state are shared in-process
	// (the entire reason for the multi-vessel daemon model).
	LLMClients map[string]llm.LLM

	// LLMLimiters is the daemon-shared map of [Limiter]
	// instances keyed by LLMProfile.metadata.name. Engine
	// factories MUST call Limiter.Acquire on the matching profile
	// before each LLM call so the daemon-wide
	// spec.llmRateLimits caps are honoured. nil entries (or a
	// nil map) mean "no limit" — factories should treat absent
	// limiters as an explicit permit.
	LLMLimiters map[string]Limiter

	// SecretLookup resolves valueFrom.secretRef references the
	// factory still holds (e.g. when it postpones secret reads to
	// runtime). Factories that consumed all their secrets at
	// resolve time can ignore this.
	SecretLookup SecretLookup
}

// SecretLookup mirrors what the resolver passes through after
// secret docs have been merged. Implementations are in
// cmd/vesseld/resolver; the catalog package only declares the
// interface so factories can depend on the smaller surface.
type SecretLookup interface {
	// Get returns the plain-text value at secret/key, or
	// errdefs.NotFound when the secret or key is absent.
	Get(secretName, key string) (string, error)
}

// EngineFactoryFn constructs an engine.Engine from a ref-specific
// config map plus shared deps. Returning a nil engine is treated
// as a configuration error (factories should return errdefs.Validation
// or wrap a real cause instead).
type EngineFactoryFn func(ref string, cfg map[string]any, deps Deps) (engine.Engine, error)

// ProbeFactoryFn constructs a spec.Probe instance.
type ProbeFactoryFn func(ref string, cfg map[string]any, deps Deps) (spec.Probe, error)

// ToolPackFactoryFn constructs a list of [tool.Tool] implementations
// for the given ref+config. Returned tools are added to the daemon-
// shared tool.Registry by the resolver; factories MUST return
// stable Definition().Name values across calls so duplicate
// invocations resolve to the same tool ids.
type ToolPackFactoryFn func(ref string, cfg map[string]any, deps Deps) ([]tool.Tool, error)

// LLMProviderFactoryFn normalises an LLMProfile document into a
// [llm.ProviderConfig] suitable for the daemon-shared
// [llm.LLMResolver]. The factory does not construct an llm.LLM
// directly — it only translates the YAML config (defaultModel,
// baseURL, organisation id, ...) plus the resolved API key into
// the provider-specific Config map the underlying llm.ProviderFactory
// already knows how to consume.
//
// Returning an error here means the provider name is registered but
// the per-profile configuration is invalid (e.g. unknown config
// key, type mismatch). Use errdefs.Validation so the resolver can
// surface the file:line uniformly.
type LLMProviderFactoryFn func(profileName string, profileCfg map[string]any, apiKey string) (llm.ProviderConfig, error)

// HistoryFactoryFn constructs a history.History from the ref+config.
type HistoryFactoryFn func(ref string, cfg map[string]any, deps Deps) (history.History, error)

// Catalog holds the per-category factory maps. Methods are
// goroutine-safe so future hot-plug work (loading a Go plugin at
// runtime) can register without a daemon restart.
type Catalog struct {
	mu sync.RWMutex

	engines      map[string]EngineFactoryFn
	probes       map[string]ProbeFactoryFn
	toolPacks    map[string]ToolPackFactoryFn
	llmProviders map[string]LLMProviderFactoryFn
	histories    map[string]HistoryFactoryFn
}

// New returns an empty Catalog. Most callers want [Builtin] which
// pre-populates v0.1.0 in-tree factories; New is exposed primarily
// for tests that want a clean slate.
func New() *Catalog {
	return &Catalog{
		engines:      map[string]EngineFactoryFn{},
		probes:       map[string]ProbeFactoryFn{},
		toolPacks:    map[string]ToolPackFactoryFn{},
		llmProviders: map[string]LLMProviderFactoryFn{},
		histories:    map[string]HistoryFactoryFn{},
	}
}

// RegisterEngine registers an engine factory under name. Re-registering
// the same name overwrites — this is intentional so a fork of vesseld
// can replace a built-in factory without forking the catalog package.
func (c *Catalog) RegisterEngine(name string, fn EngineFactoryFn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.engines[name] = fn
}

// RegisterProbe registers a probe factory under name.
func (c *Catalog) RegisterProbe(name string, fn ProbeFactoryFn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes[name] = fn
}

// RegisterToolPack registers a tool-pack factory under name.
func (c *Catalog) RegisterToolPack(name string, fn ToolPackFactoryFn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolPacks[name] = fn
}

// RegisterLLMProvider registers a provider factory under name.
func (c *Catalog) RegisterLLMProvider(name string, fn LLMProviderFactoryFn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.llmProviders[name] = fn
}

// RegisterHistory registers a history factory under name.
func (c *Catalog) RegisterHistory(name string, fn HistoryFactoryFn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.histories[name] = fn
}

// Engine looks up the engine factory or returns errdefs.NotFound.
func (c *Catalog) Engine(name string) (EngineFactoryFn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn, ok := c.engines[name]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld catalog: engine ref %q not registered (known: %v)", name, sortedKeys(c.engines))
	}
	return fn, nil
}

// Probe looks up the probe factory or returns errdefs.NotFound.
func (c *Catalog) Probe(name string) (ProbeFactoryFn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn, ok := c.probes[name]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld catalog: probe ref %q not registered (known: %v)", name, sortedKeys(c.probes))
	}
	return fn, nil
}

// ToolPack looks up the tool-pack factory or returns errdefs.NotFound.
func (c *Catalog) ToolPack(name string) (ToolPackFactoryFn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn, ok := c.toolPacks[name]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld catalog: toolpack ref %q not registered (known: %v)", name, sortedKeys(c.toolPacks))
	}
	return fn, nil
}

// LLMProvider looks up the provider factory or returns errdefs.NotFound.
func (c *Catalog) LLMProvider(name string) (LLMProviderFactoryFn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn, ok := c.llmProviders[name]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld catalog: llm provider %q not registered (known: %v)", name, sortedKeys(c.llmProviders))
	}
	return fn, nil
}

// History looks up the history factory or returns errdefs.NotFound.
func (c *Catalog) History(name string) (HistoryFactoryFn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn, ok := c.histories[name]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld catalog: history ref %q not registered (known: %v)", name, sortedKeys(c.histories))
	}
	return fn, nil
}

// Names returns the sorted ref names for one category. Useful for
// CLI help and `vesseld plan` output.
func (c *Catalog) Names(category string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch category {
	case "engines":
		return sortedKeys(c.engines)
	case "probes":
		return sortedKeys(c.probes)
	case "toolpacks":
		return sortedKeys(c.toolPacks)
	case "llmProviders":
		return sortedKeys(c.llmProviders)
	case "histories":
		return sortedKeys(c.histories)
	default:
		return nil
	}
}

// sortedKeys is the small helper used by every error message and
// Names() call so error output is stable across runs.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings is a tiny insertion sort to avoid pulling in the sort
// package for one call site. Catalog maps are small (<20 entries)
// so the asymptotic cost is irrelevant.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// formatRefError is the small wrapper used by factory bodies to
// produce a uniform "<category> <ref>: <reason>" prefix; saves
// every factory from re-spelling the format string.
func formatRefError(category, ref, format string, args ...any) error {
	return errdefs.Validationf("%s %s: %s", category, ref, fmt.Sprintf(format, args...))
}
