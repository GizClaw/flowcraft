package vessel

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// Option configures a [Captain] at construction time. Options compose
// freely; later options override earlier ones for single-valued
// dimensions, append for collections.
type Option func(*config)

// EngineFactory builds an [engine.Engine] for the given Agent entry,
// receiving the resolved dependencies the Captain has hydrated. The
// factory is called once per agent at New time and the resulting
// engine is reused for every Run dispatched to that agent.
//
// Implementations typically pick the engine kind off
// spec.EngineKind and assemble the runtime — for example, building
// a sdk/graph/runner.Engine over an llmnode that pulls its LLM from
// deps.LLMResolver and its tools (filtered by spec.Tools) from
// deps.ToolRegistry. The factory is the single seam where
// caller-side wiring meets the vessel runtime.
type EngineFactory func(aspec spec.Agent, deps Deps) (engine.Engine, error)

// Deps is the dependency bundle handed to [EngineFactory]. Every
// field is optional — a factory that needs none of them is fine —
// but using these accessors instead of capturing them in a closure
// lets callers reuse a single factory across multiple vessels with
// different dependency wiring.
type Deps struct {
	// ToolRegistry is the [tool.Registry] callers passed via
	// [WithToolRegistry]. nil when no registry was wired. Factories
	// implementing the Tools allow-list filter inside this struct
	// (the Captain does not pre-filter so factories see the full
	// catalog and can decide whether to honour allow-listing).
	ToolRegistry *tool.Registry

	// LLMResolver is the [llm.LLMResolver] callers passed via
	// [WithLLMResolver]. nil when no resolver was wired. Factories
	// typically resolve their default model up-front and cache the
	// resulting [llm.LLM] for subsequent runs.
	LLMResolver llm.LLMResolver

	// Bus is the vessel's event.Bus. Factories MAY publish their
	// own envelopes onto it; the bus survives the Captain's
	// lifetime when callers passed one via [WithBus], otherwise
	// the Captain owns and closes it.
	Bus event.Bus

	// History is the resolved shared history (if any). Factories
	// typically do NOT need this directly: the Captain installs a
	// BoardSeeder + Observer pair on every Run that uses the
	// history. It is exposed here for advanced cases — e.g. a
	// custom node that reads a snapshot at runtime.
	History history.History

	// CheckpointStore is the persistence backend wired via
	// [WithCheckpointStore]. Nil when none was configured. Most
	// factories don't need it (the sandbox host already routes
	// engine.Checkpointer.Checkpoint into the store). It is
	// exposed here for engines that want to call Load() during
	// EngineFunc setup to resume a previous run.
	CheckpointStore engine.CheckpointStore
}

// WithEngine wires a single, pre-constructed engine that every agent
// shares. Convenience for vessels whose agents all run the same
// graph; callers needing per-agent engines should use
// [WithEngineFactory].
//
// The supplied engine.Engine MUST honour the engine.Engine contract
// (run-to-completion semantics, ctx cancellation, interrupt
// handling). Use sdk/graph/runner for production graphs or
// engine.EngineFunc for tests.
func WithEngine(eng engine.Engine) Option {
	return func(c *config) {
		c.engineFactory = func(_ spec.Agent, _ Deps) (engine.Engine, error) {
			return eng, nil
		}
	}
}

// WithEngineFactory installs a custom factory that the Captain
// invokes once per agent at New time. This is the primary extension
// point for vessels that mix engine kinds across agents.
func WithEngineFactory(f EngineFactory) Option {
	return func(c *config) { c.engineFactory = f }
}

// WithHost installs the base [engine.Host] handed to every agent
// run. The Captain wraps this host with its own sandbox that
// enforces [spec.Resources] and emits engine envelopes onto
// the vessel bus — callers therefore only need to provide the
// host capabilities the engine itself relies on (UserPrompter,
// Checkpointer, UsageReporter, …).
//
// When omitted, the Captain falls back to engine.NoopHost embedded
// inside the sandbox.
func WithHost(h engine.Host) Option {
	return func(c *config) { c.engineHost = h }
}

// WithBus installs the event.Bus used to fan out engine envelopes
// (run / step / stream.delta) and vessel-level lifecycle events
// (vessel.phase.changed). When omitted, the Captain creates an
// event.NewMemoryBus internally and closes it on Stop.
//
// Supplying a bus is the seam by which higher layers (sdk/a2a SSE
// bridge, dashboard, log shippers) plug into the vessel without
// observing the Captain directly.
func WithBus(bus event.Bus) Option {
	return func(c *config) {
		c.bus = bus
		c.busOwned = false
	}
}

// WithObserver registers an agent.Observer that fires on every Run
// dispatched through the vessel. Observers fire in registration
// order, after any [agent.Agent.Observers] declared on the
// per-agent value the Captain assembles internally.
func WithObserver(o agent.Observer) Option {
	return func(c *config) {
		if o != nil {
			c.observers = append(c.observers, o)
		}
	}
}

// WithDecider registers an agent.Decider for every Run.
func WithDecider(d agent.Decider) Option {
	return func(c *config) {
		if d != nil {
			c.deciders = append(c.deciders, d)
		}
	}
}

// WithToolRegistry wires the [tool.Registry] the Captain hands to
// the [EngineFactory] via [Deps.ToolRegistry]. The vessel itself
// only uses the registry indirectly: it forwards the per-agent
// allow-list to the factory, which is responsible for honouring it
// when building the engine's tool wiring.
func WithToolRegistry(r *tool.Registry) Option {
	return func(c *config) { c.toolRegistry = r }
}

// WithLLMResolver wires the [llm.LLMResolver] the Captain hands to
// the [EngineFactory] via [Deps.LLMResolver]. The resolver is also
// used by built-in probes (LLMReachableProbe) when the spec
// declares them.
func WithLLMResolver(r llm.LLMResolver) Option {
	return func(c *config) { c.llmResolver = r }
}

// WithHistory overrides the History that the Captain would
// otherwise build from spec.History. Use this when the caller wants
// to share a single history.History across multiple vessels (e.g.
// a tenant-scoped store) instead of letting each vessel own its own.
//
// When the spec also declares spec.History, WithHistory wins. When
// the spec declares no history but WithHistory is set, the supplied
// history is wired anyway — this is the recommended way to attach
// a history.History implementation that vesselspec cannot describe
// (custom backends, archival pipelines).
func WithHistory(h history.History) Option {
	return func(c *config) { c.historyOverride = h }
}

// WithCheckpointStore wires an [engine.CheckpointStore] that the
// Captain hooks into the sandbox host. Every engine that calls
// host.Checkpoint(ctx, cp) inside a Run goes through this store —
// factories therefore do NOT need to plumb a Checkpointer
// themselves; the contract is "if WithCheckpointStore is set,
// engines that emit checkpoints persist them, otherwise they are
// silently dropped (NoopCheckpointStore)".
//
// v0.1.0 ships only [engine.NoopCheckpointStore] in-tree; the
// Postgres / SQLite / Redis implementations land in v0.2.0 and
// remain wire-compatible with this Option.
//
// The store is also exposed via [Deps.CheckpointStore] so engines
// that want to call Load (resume from prior state) at construction
// time can. Save / Delete go through the host wrapper.
func WithCheckpointStore(store engine.CheckpointStore) Option {
	return func(c *config) { c.checkpointStore = store }
}

// WithSessionStore wires the [SessionStore] used to provision a
// per-run [workspace.Workspace] for every dispatched agent.Run.
//
// When set, the Captain calls store.Open(runCtx, runID) at the start
// of Submit and stashes the returned Workspace onto runCtx so engines
// / tools can reach it via [WorkspaceFromContext]; store.Close is
// invoked on the vessel baseCtx when the run terminates so cleanup
// survives a runCtx cancellation.
//
// When omitted, [WorkspaceFromContext] returns (nil, false) inside
// every run — tools that need a workspace must fall back to their
// own wiring. There is intentionally no default SessionStore: making
// every Captain own a temporary directory would surprise callers who
// never asked for one, and the choice between in-memory vs. on-disk
// is workload-dependent.
func WithSessionStore(store SessionStore) Option {
	return func(c *config) { c.sessionStore = store }
}

// WithBaseContext sets the parent context every Submit / Call
// inherits from. The default is context.Background(); supplying a
// scoped parent lets the caller propagate a tenant / request id
// through to the agent without re-plumbing it on every Submit.
//
// The base context is propagated as the seed for each Run; the
// per-Submit ctx (whose cancellation drives in-flight cancellation)
// is composed atop it.
func WithBaseContext(ctx context.Context) Option {
	return func(c *config) {
		if ctx != nil {
			c.baseCtx = ctx
		}
	}
}

// config holds the resolved options. Kept private so additions
// remain non-breaking — callers only see Option.
type config struct {
	engineFactory EngineFactory
	engineHost    engine.Host
	bus           event.Bus
	busOwned      bool

	observers []agent.Observer
	deciders  []agent.Decider

	toolRegistry    *tool.Registry
	llmResolver     llm.LLMResolver
	historyOverride history.History
	checkpointStore engine.CheckpointStore
	sessionStore    SessionStore

	baseCtx context.Context
}
