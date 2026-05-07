package spec

import "time"

// Spec is the declarative description of a Vessel: which agents it
// hosts, how memory is shared across them, what resources it may
// consume, and how the controller reacts to failure.
//
// Spec is data-only — turning it into a running vessel is the
// Captain's job in the vessel runtime module. All optional fields
// zero-value to "feature disabled / use default", so the smallest
// valid Spec is a single Agent with a non-empty Name.
//
// # Forward compatibility
//
// Fields are additive across releases. v0.1.0 deliberately exposes
// only fields the runtime can honour today; entries for token
// budgets, secret references, sidecar producer chains, and the
// "always" restart mode will appear in later versions when the
// vessel runtime grows support for them.
type Spec struct {
	// ID is the vessel identifier surfaced on the control plane
	// and in observability attributes. Empty IDs are auto-generated
	// by the Captain at construction time using the same scheme
	// agent.Run uses for run ids.
	ID string `json:"id,omitempty" yaml:"id,omitempty"`

	// Agents lists the agent payloads the vessel hosts. At least
	// one entry is mandatory: a vessel without agents has no work
	// to do. The first non-Sidecar entry is the "primary" agent
	// the bus subscription routing falls back to when an agent
	// name lookup fails.
	Agents []Agent `json:"agents" yaml:"agents"`

	// History describes how the vessel's shared transcript is
	// stored and which strategy is used to assemble it for each
	// agent. nil means "no shared history" — agents start from a
	// fresh transcript every turn.
	History *History `json:"history,omitempty" yaml:"history,omitempty"`

	// Resources caps vessel-wide consumption (in-flight runs,
	// per-turn timeout). The zero value means "no limits". The
	// Captain enforces these via its sandbox host.
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Probes is the optional health-check configuration the Captain
	// runs in the background. nil disables the probe loop. v0.1.0
	// supports liveness-style probes only; readiness gating lands
	// in a later release.
	Probes *Probes `json:"probes,omitempty" yaml:"probes,omitempty"`

	// Restart controls how the Captain reacts to a fatal phase
	// transition (probe failure). The zero value is RestartNever
	// (caller-controlled recovery).
	Restart Restart `json:"restart,omitempty" yaml:"restart,omitempty"`

	// Kanban opts the vessel into multi-agent collaboration via
	// the Kanban board: Dispatcher agents gain auto-injected
	// kanban_submit / task_context tools, and the callback bridge
	// turns Card terminations into "[Task Callback]" messages on
	// the dispatcher's history. nil disables the Kanban subsystem
	// entirely; in that mode Agent.Dispatcher must remain false
	// or validation rejects the spec.
	Kanban *Kanban `json:"kanban,omitempty" yaml:"kanban,omitempty"`

	// Labels / Annotations classify vessels for observability and
	// the management API. Labels are intended for selectors (low
	// cardinality, alphanumeric); Annotations are unstructured
	// metadata that the Captain echoes verbatim onto every emitted
	// envelope.
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// Agent describes one agent inside a vessel. The Captain assembles
// the corresponding sdk/agent.Agent value at New time using these
// fields plus the dependencies passed via vessel runtime options
// (engine factory, tool registry, llm resolver).
//
// Note: spec.Agent and sdk/agent.Agent are deliberately
// distinct types — the former is the declarative entry inside a
// Spec, the latter is the runtime value the engine receives. Same
// short name, different package qualifiers; readers disambiguate
// via the import qualifier.
type Agent struct {
	// Name uniquely identifies the agent within the vessel. It MUST
	// be non-empty and stable across restarts — telemetry, history
	// conversation keys, and the agentName argument of Submit / Call
	// all use it.
	Name string `json:"name" yaml:"name"`

	// Card is the A2A-compatible agent card forwarded onto the
	// resulting sdk/agent.Agent.Card. Optional.
	//
	// The card type is intentionally `any` here so vesselspec does
	// not import sdk/agent (avoids the dependency inversion called
	// out in the package doc); the vessel runtime asserts the
	// concrete sdk/agent.AgentCard at assembly time.
	Card any `json:"card,omitempty" yaml:"card,omitempty"`

	// EngineKind selects which engine implementation runs the
	// agent. v0.1.0 of the vessel runtime accepts "graph" (the
	// default when empty); custom kinds are routable via an
	// EngineFactory option but must still pass validation here.
	EngineKind string `json:"engine_kind,omitempty" yaml:"engine_kind,omitempty"`

	// Tools lists the tool ids the agent is permitted to call. The
	// Captain looks them up in the configured tool registry and
	// passes the filtered list onto sdk/agent.Agent.Tools. Empty
	// means "no tool gate" — the engine sees the agent's full
	// declared toolset.
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`

	// HistoryAccess controls the agent's relationship with the
	// shared History. Defaults to HistoryAccessReadWrite when
	// Spec.History is set; ignored when no History is configured.
	HistoryAccess HistoryAccess `json:"history_access,omitempty" yaml:"history_access,omitempty"`

	// Sidecar marks an agent that is triggered by event.Bus
	// subscription rather than by direct Submit / Call. When true,
	// SubscribeTo MUST be set to a valid event.Pattern; the Captain
	// runs the agent every time a matching envelope is published.
	// Submit / Call still work and behave the same way as for any
	// agent, providing a manual trigger for tests / replay.
	Sidecar bool `json:"sidecar,omitempty" yaml:"sidecar,omitempty"`

	// SubscribeTo is the event.Pattern (NATS-style; see
	// sdk/event.Pattern) the sidecar agent listens on. Ignored
	// when Sidecar is false. Empty when Sidecar is true is a
	// validation error.
	SubscribeTo string `json:"subscribe_to,omitempty" yaml:"subscribe_to,omitempty"`

	// Dispatcher marks an agent that is allowed to delegate work
	// to other agents in the same vessel via the Kanban board.
	// When true, the Captain auto-injects the Kanban submit /
	// task-context tools into the agent's runtime tool set, and
	// the callback bridge appends "[Task Callback]" messages onto
	// the agent's history when its dispatched cards terminate so
	// the next turn sees the result.
	//
	// Dispatcher REQUIRES Spec.Kanban to be non-nil; the Captain
	// has nothing to dispatch through otherwise. Sidecar agents
	// are allowed to be Dispatchers — a bus-triggered analyst
	// can still hand follow-up work to other agents — but the
	// most common pattern is a foreground Dispatcher backed by
	// one or more worker agents.
	Dispatcher bool `json:"dispatcher,omitempty" yaml:"dispatcher,omitempty"`

	// ProducerChain caps how deep a chain of dispatched tasks may
	// grow before the Captain refuses further submissions from
	// this agent. The depth is carried in ctx via
	// kanban.WithProducerID and incremented on every nested
	// dispatch — A submits to B (depth 1), B (running as the
	// dispatched task) submits to C (depth 2), and so on.
	//
	// 0 means "use Spec.Kanban.MaxProducerChain". Setting a small
	// per-agent value is the recommended way to disable nested
	// dispatch on workers that are not supposed to delegate
	// further (set ProducerChain=1 on a leaf worker so it can
	// receive tasks but cannot recurse).
	ProducerChain int `json:"producer_chain,omitempty" yaml:"producer_chain,omitempty"`
}

// History selects the shared-history strategy used by every agent
// in the vessel. nil at the Spec level means "no shared history";
// per-agent opt-out lives on Agent.HistoryAccess.
type History struct {
	// Kind chooses the strategy: "buffer" (sdk/history.NewBuffer)
	// or "compacted" (sdk/history.NewCompacted). Empty defaults
	// to "buffer".
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// MaxMessages caps the message count for buffer histories. 0
	// uses the underlying default.
	MaxMessages int `json:"max_messages,omitempty" yaml:"max_messages,omitempty"`

	// TokenBudget caps the token count for compacted histories. 0
	// uses the underlying default.
	TokenBudget int `json:"token_budget,omitempty" yaml:"token_budget,omitempty"`
}

// HistoryAccess enumerates an agent's permissions on the shared
// History. The default is HistoryAccessReadWrite when a History is
// configured.
type HistoryAccess string

const (
	// HistoryAccessNone disables history wiring for this agent.
	// Useful for tool-only agents whose runs should not pollute
	// the shared transcript.
	HistoryAccessNone HistoryAccess = "none"

	// HistoryAccessReadOnly seeds the board from history but does
	// not append the agent's output back. Moderator / analytics
	// sidecars typically want this; the Captain uses
	// history.LoadFiltered when assembling the seeded transcript
	// so callers can scope what the read-only agent observes.
	HistoryAccessReadOnly HistoryAccess = "read_only"

	// HistoryAccessReadWrite seeds and appends. The default for
	// primary agents.
	HistoryAccessReadWrite HistoryAccess = "read_write"
)

// Resources caps vessel-wide consumption. Zero-value fields mean
// "no limit on this dimension".
//
// # v0.1.0 surface
//
// MaxConcurrentRuns / TurnTimeout are admission-gated up-front.
// MaxTokensPerTurn / MaxTokensPerHour are token-budget caps fed by
// the engine's [engine.UsageReporter]; the Captain accumulates
// totals per Run and per rolling-hour window and aborts a Run with
// [errdefs.RateLimit] once either threshold is crossed. Engines
// that never call ReportUsage cannot trip these gates; the budget
// is therefore opt-in from the engine side.
type Resources struct {
	// MaxConcurrentRuns caps how many runs may execute in parallel
	// across all agents. 0 = unlimited. The Captain semaphore-gates
	// Submit so a vessel cannot exceed the cap even under burst.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty" yaml:"max_concurrent_runs,omitempty"`

	// TurnTimeout aborts any single Submit/Call that exceeds the
	// duration. 0 = no timeout (rely on caller-supplied ctx). The
	// timer is per-Run, not per-vessel, so multiple Submits each
	// get their own budget.
	TurnTimeout time.Duration `json:"turn_timeout,omitempty" yaml:"turn_timeout,omitempty"`

	// MaxTokensPerTurn caps the total tokens (prompt + completion)
	// a single Run may consume. 0 = unlimited. The cap is checked
	// each time the engine calls UsageReporter.ReportUsage; the run
	// context is cancelled with [errdefs.RateLimit] once exceeded.
	MaxTokensPerTurn int64 `json:"max_tokens_per_turn,omitempty" yaml:"max_tokens_per_turn,omitempty"`

	// MaxTokensPerHour caps the rolling-window vessel-wide total.
	// 0 = unlimited. Implemented as a 60-bucket sliding window so
	// the cap is enforced with ~1-minute granularity. Submits past
	// the budget are rejected up-front with [errdefs.RateLimit];
	// in-flight runs that push over mid-execution are cancelled
	// at their next ReportUsage call.
	MaxTokensPerHour int64 `json:"max_tokens_per_hour,omitempty" yaml:"max_tokens_per_hour,omitempty"`
}

// Restart controls how the Captain recovers from a probe-driven
// transition to PhaseFailed. Zero value is RestartNever — the
// caller observes the failure via Captain.Phase and decides what to
// do.
//
// v0.1.0 supports RestartNever and RestartOnFailure with a simple
// exponential backoff. The "always" mode and richer policies (jitter,
// per-failure type) will land in a later release alongside richer
// probe primitives.
type Restart struct {
	// Mode picks the policy. Empty defaults to RestartNever.
	Mode RestartMode `json:"mode,omitempty" yaml:"mode,omitempty"`

	// BackoffInit is the wait before the first restart attempt.
	// Defaults to one second when zero.
	BackoffInit time.Duration `json:"backoff_init,omitempty" yaml:"backoff_init,omitempty"`

	// BackoffMax caps the exponential backoff. Defaults to one
	// minute when zero.
	BackoffMax time.Duration `json:"backoff_max,omitempty" yaml:"backoff_max,omitempty"`

	// MaxRestarts limits the number of restart attempts before the
	// Captain transitions permanently to PhaseFailed. 0 = unlimited.
	MaxRestarts int `json:"max_restarts,omitempty" yaml:"max_restarts,omitempty"`
}

// Kanban opts the vessel into multi-agent collaboration via the
// Kanban board (sdk/kanban). Setting Spec.Kanban to a non-nil value
// turns on the Captain's Kanban subsystem; the zero value is
// "Kanban disabled".
//
// Multi-agent without Kanban is still possible (caller-orchestrated
// chains using shared history, or Sidecar agents reacting to bus
// envelopes) — Kanban specifically enables the **agent-as-tool**
// pattern: an LLM in one agent calls kanban_submit("worker", ...)
// to delegate a task and receives the result asynchronously via a
// "[Task Callback]" message on its next turn.
type Kanban struct {
	// MaxPendingTasks caps the in-flight task queue depth. 0 uses
	// the kanban package default. Submissions beyond the cap fail
	// with errdefs.RateLimit.
	MaxPendingTasks int `json:"max_pending_tasks,omitempty" yaml:"max_pending_tasks,omitempty"`

	// MaxProducerChain is the default cap on dispatch recursion
	// depth used when Agent.ProducerChain is unset. 0 defaults to
	// 8 — deep enough for legitimate Plan→Research→Verify chains,
	// shallow enough to break runaway loops fast.
	MaxProducerChain int `json:"max_producer_chain,omitempty" yaml:"max_producer_chain,omitempty"`

	// CallbackMaxSummary caps the prefix length of a worker's
	// output that is included in the "[Task Callback]" message
	// fed back to the dispatcher. 0 uses the kanban package
	// default (200 chars). Dispatchers can always call
	// task_context(card_id) to retrieve the full result.
	CallbackMaxSummary int `json:"callback_max_summary,omitempty" yaml:"callback_max_summary,omitempty"`
}

// RestartMode enumerates the high-level restart strategy. v0.1.0
// supports the two listed values.
type RestartMode string

const (
	// RestartNever leaves recovery to the caller. The default.
	RestartNever RestartMode = "never"

	// RestartOnFailure restarts only when the Captain transitions
	// to PhaseFailed (probe failure). Successful drains do NOT
	// trigger a restart.
	RestartOnFailure RestartMode = "on_failure"
)
