// Package depname enumerates the conventional string identifiers
// engines use when declaring [engine.Capabilities.RequiredDepNames]
// and when looking values up in [engine.Dependencies].
//
// # Why a string convention
//
// [engine.Dependencies] is keyed by any so the container imposes no
// vocabulary of its own. That keeps the type system honest — Get
// returns a typed value via [engine.GetDep] — but it also means
// engines and hosts need an out-of-band agreement on which key holds
// which thing.
//
// depname is that agreement, expressed as plain strings:
//
//   - JSON / YAML / OTel-attribute friendly: a Capability declared as
//     RequiredDepNames=[]string{depname.LLMClient} round-trips through
//     dashboards, admin APIs, and pod controllers without Go-type
//     marshalling tricks.
//   - Constant identifiers prevent typos at compile time.
//   - Third-party packages can extend the catalog by introducing
//     their own depname sub-package (e.g. sdk-x/redis/depname) without
//     touching this file.
//
// # Convention vs registration
//
// Naming a constant here does NOT auto-populate the container. The
// host is responsible for calling deps.Set(depname.X, value) before
// invoking the engine; engines call engine.GetDep[T](deps, depname.X)
// to retrieve. depname only standardises the spelling.
//
// # Capability declarations
//
// Engines that need a dependency declare it via:
//
//	func (e *MyEngine) Capabilities() engine.Capabilities {
//	    return engine.Capabilities{
//	        RequiredDepNames: []string{
//	            depname.LLMClient,
//	            depname.ToolRegistry,
//	        },
//	    }
//	}
//
// agent.Run / vessel build paths that perform pre-flight validation
// iterate RequiredDepNames and reject the run when a required key is
// absent in the container — surfacing wiring mistakes before any
// engine.Execute call.
//
// # Naming convention
//
// All constants follow the pattern <package>.<noun> in lower-snake-
// joined-by-dot form. Pick the most specific package that owns the
// concept (sdk/llm owns llm.client even though scriptengine also
// uses it). Avoid abbreviations.
package depname

const (
	// LLMClient is a single fully-constructed llm.LLM instance the
	// engine should use for model inference. The underlying value
	// stored in Dependencies MUST satisfy llm.LLM.
	LLMClient = "llm.client"

	// LLMResolver is an llm.LLMResolver — a per-model lookup the
	// engine consults when the model id is selected at run time
	// (graph node config, script var, agent overlay, …) rather
	// than baked in at engine construction time. The underlying
	// value MUST satisfy llm.LLMResolver.
	LLMResolver = "llm.resolver"

	// ToolRegistry is a *tool.Registry the engine queries for
	// tool definitions and uses to execute model-issued tool
	// calls. The underlying value MUST be *tool.Registry.
	ToolRegistry = "tool.registry"

	// ToolAllowedNames is the agent-level tool allow-list — the
	// strict set of tool ids the engine is permitted to expose to
	// the model and to execute. Empty slice means "no tools
	// permitted" (fail-closed). The underlying value MUST be a
	// []string.
	//
	// agent.Run populates this from agent.Agent.Tools when the
	// caller has not already set it on a custom Dependencies
	// container — caller-supplied wins.
	ToolAllowedNames = "tool.allowed_names"

	// HistoryStore is a history.History the engine MAY use to
	// load / append the conversation transcript. Engines that
	// receive their messages exclusively through the seeded
	// engine.Board do not need this dep — it is exposed for
	// engines that fetch / persist history themselves (e.g. a
	// scriptengine that supports ad-hoc history.search calls).
	// The underlying value MUST satisfy history.History.
	HistoryStore = "history.store"
)
