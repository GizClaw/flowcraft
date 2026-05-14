package resolver

import (
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// Plan is the resolved runtime configuration the daemon executes.
// The struct is the canonical artefact passed from the resolver
// (which only does config-time work) to the fleet (which spins up
// runtime objects). Plan instances are immutable after Resolve;
// every field is either a value or a pointer to an already-built
// shared resource.
type Plan struct {
	// Daemon carries the resolved daemon-level configuration:
	// listener parameters, drain timeout, daemon-wide concurrency
	// cap, logging.
	Daemon DaemonPlan

	// Vessels is the ordered list of resolved Vessels. Order
	// follows v1alpha1 document order; the fleet honours it for
	// startup ordering so dependent vessels (e.g. one that hosts
	// a sidecar listening to another's bus envelopes) can rely on
	// deterministic launch order.
	Vessels []VesselPlan

	// SharedToolRegistry is the daemon-shared registry where
	// every ToolPack-supplied tool is registered. Captains share
	// it so two Vessels referencing the same ToolPack get the
	// same tool implementations (no per-vessel re-registration).
	SharedToolRegistry *tool.Registry

	// SharedLLMResolver is the daemon-wide llm.LLMResolver. Every
	// LLMProfile.metadata.name surfaces as a resolvable model id;
	// the resolver caches LLM client instances internally so
	// repeated Resolve() calls reuse the underlying connection
	// pool and rate-limit state.
	SharedLLMResolver llm.LLMResolver

	// SharedHistories maps HistoryStore.metadata.name → resolved
	// history.History instance. Fleet looks the right one up by
	// name when constructing each Captain.
	SharedHistories map[string]history.History
}

// DaemonPlan is the resolved Daemon document.
type DaemonPlan struct {
	Name              string
	Socket            string
	Listen            string
	TokenFile         string
	MTLS              *DaemonMTLSPlan
	MaxConcurrentRuns int
	LLMRateLimits     []v1alpha1.LLMRateLimit
	LoggingFormat     string
	LoggingLevel      string
	DrainTimeout      time.Duration
}

// DaemonMTLSPlan is the resolved mTLS configuration. The Ref
// fields use the secrets.Provider URL-keyed syntax (env://...,
// file:///..., vault://...); the runtime layer is responsible for
// fetching the actual PEM bytes via the daemon-wide Provider when
// constructing the tls.Config.
type DaemonMTLSPlan struct {
	CertRef     string
	KeyRef      string
	ClientCARef string
	// MinVersion is "1.2" or "1.3" (defaulted to "1.3" by Resolve
	// if the apispec field was empty).
	MinVersion string
}

// VesselPlan is one resolved Vessel: enough information for the
// fleet to call vessel.New + Captain.Launch without consulting
// the resolver again.
type VesselPlan struct {
	// Name is the user-facing vessel id (HTTP path component,
	// telemetry attribute).
	Name string

	// Spec is the spec.Spec the Captain consumes. The
	// Probe / EngineFactory closures are NOT carried here — they
	// live in EngineFactories / Probes below so the Plan stays
	// JSON-serialisable for `vesseld plan` rendering.
	Spec spec.Spec

	// HistoryName is the resolved HistoryStore name (empty when
	// the vessel has no history). Fleet looks the implementation
	// up in Plan.SharedHistories.
	HistoryName string

	// EngineFactoriesByAgent holds one engine builder per agent.
	// The fleet wires this into vessel.WithEngineFactory so each
	// agent gets its declared engine ref + config bound at
	// vessel construction time.
	EngineFactoriesByAgent map[string]EngineBuilder

	// EngineRefByAgent records the apispec engine ref string per
	// agent so read-only API surfaces (the /plan endpoint) can
	// answer "which engine does agent X use?" without having to
	// reflect into the EngineBuilder closures. Populated by the
	// resolver alongside EngineFactoriesByAgent.
	EngineRefByAgent map[string]string

	// Probes holds the resolved probe instances keyed by name.
	// Order does not matter — the spec.Probes.Liveness slice
	// in Spec carries the order, and the Captain looks each
	// probe up by name from this map.
	Probes map[string]spec.Probe

	// SidecarSubscribeBy holds the SubscribeTo pattern per sidecar
	// agent name. Mirrors data already present in Spec.Agents
	// but kept here for the fleet to wire bus subscriptions
	// without re-walking the spec.
	SidecarSubscribeBy map[string]string

	// DispatcherAgents lists agent names with Dispatcher=true.
	// Cached so the fleet does not repeatedly walk the spec.
	DispatcherAgents []string
}

// RuntimeDeps carries the per-call inputs the fleet hands to an
// EngineBuilder at construction time. Fields here are intentionally
// the ones that are NOT known at resolve time:
//
//   - AgentTools is the resolved allow-list FOR THIS Captain build,
//     including any vessel-runtime augmentation (e.g. kanban tools
//     auto-injected for Dispatcher agents). Captured at fleet build
//     time so the resolver-side closure does not need to mirror the
//     vessel runtime's per-agent augmentation rules.
//   - LLMLimiters is the daemon-shared rate-limiter map keyed by
//     LLMProfile name. Lives in the fleet so its in-memory token
//     buckets can be reused across multiple Captain rebuilds (e.g.
//     during restart loops); the resolver only sees the static
//     LLMRateLimit configuration, not the live buckets.
type RuntimeDeps struct {
	AgentTools  []string
	LLMLimiters map[string]catalog.Limiter
}

// EngineBuilder is the resolved engine factory closure, already
// bound to its ref string + config map. The fleet calls it from
// inside its EngineFactory closure once per Captain build, passing
// the fleet-level RuntimeDeps that the resolver could not capture.
type EngineBuilder func(rd RuntimeDeps) (engineBuildResult, error)

// engineBuildResult bundles the engine + the per-agent metadata
// the fleet needs to set on spec.Agent before passing to
// vessel.New. Internal because callers always go through
// EngineBuilder rather than constructing this directly.
type engineBuildResult struct {
	// Engine is the runnable engine.Engine the agent will use.
	// Carried as `any` to avoid pulling sdk/engine into the
	// resolver package's exported surface; the fleet asserts the
	// concrete type at use time.
	Engine any
}

// MarshalRedacted returns a deep copy of the plan with secret
// values replaced by "[REDACTED]". Used by `vesseld plan` to
// render the resolved configuration without leaking credentials.
//
// Currently the only secret-bearing fields are LLMProfile auth
// values, and the resolver consumes those during construction
// (the resolved api keys live inside closures, not in the Plan
// struct). MarshalRedacted is therefore a no-op today; it exists
// as the documented seam for v0.2.0+ when secrets reach further
// fields (e.g. token-file content, TLS material).
func (p Plan) MarshalRedacted() Plan { return p }
