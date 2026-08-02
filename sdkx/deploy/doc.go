// Package deploy assembles a whole deployment from one declarative
// YAML document: a resource area of shared, named objects, and the
// agents that bind them.
//
// It is the mechanism layer and imports no module config package.
// inference, tool, workspace and sandbox each plug in as one
// registered impl, so a deployment links only the integrations it
// actually names — the same opt-in rule sdkx/tool/config applies to
// MCP. Values YAML cannot express arrive through registered
// constructors and sources supplied by the application.
//
// The document is PURE YAML end to end — every settings block is
// decoded by yaml.v3 into native Go values or yamlv3.Node; no JSON
// intermediate ever appears. Unknown fields are rejected at parse
// time so typos surface immediately.
//
// # Document shape
//
//	version: v1
//	resources:
//	  fs:
//	    kind: workspace.Registry
//	    impl: yaml
//	    settings: {file: workspace.yaml}
//	  box:
//	    kind: sandbox.Registry
//	    impl: yaml
//	    deps: {workspaces: fs}            # resources may bind resources
//	    settings: {file: sandbox.yaml}
//	  infer:
//	    kind: inference.Assembly
//	    impl: yaml
//	    settings: {file: inference.yaml}
//	  kit:
//	    kind: tool.Assembly
//	    impl: yaml
//	    deps: {sandbox: box/coding}       # one item inside a container
//	    settings: {file: tools.yaml}
//
//	agents:
//	  researcher:
//	    card: {name: 研究员, description: 深度调研}
//	    tools: [search, fetch]
//	    engine:
//	      kind: graph                     # looked up in agent.Registry
//	      settings:                       # opaque; the factory validates
//	        graph: graphs/research.json
//	    deps:                             # keyed by the factory's DepSpec
//	      inference: infer
//	      tools:     kit
//	      workspace: fs/project
//	    prepare:                          # Preparer chain, runs in order
//	      - type: recall
//	        deps: {store: mem}
//	        settings: {max_hits: 8}
//	    observe:                          # Observers, read-only
//	      - type: transcript
//	        deps: {store: mem}
//	    referees:                         # Referees, return Decision
//	      - type: discard_on_interrupt    # the one built-in kind
//	        settings: {reason: barge-in, causes: [user_input]}
//	    policy:                           # per-call harness knobs
//	      max_revise: 2
//
// # Extension kinds are host-supplied
//
// Every name in kind / impl / engine.kind / prepare[].type /
// observe[].type / referees[].type is looked up in a registry, and
// this package ships exactly one entry of its own: the referee kind
// [AfterDiscardOnInterrupt]. Everything else — resource impls,
// engine kinds, and any prepare/observe/referee type — must be
// registered by the host before Build, or the document fails with
// "is not registered".
//
// The three factory types are distinct so a factory registered
// against the wrong stage is a compile error:
//
//	b.RegisterPreparer("seed", seedFactory)     // then prepare: [{type: seed}]
//	b.RegisterObserver("audit", auditFactory)   // then observe: [{type: audit}]
//	b.RegisterReferee("policy", policyFactory)  // then referees: [{type: policy}]
//
// Lifecycle factories receive deps exactly like resources do, which
// is what lets them reach into the resource area rather than being
// mere on/off switches:
//
//	observe:
//	  - type: audit
//	    deps: {workspace: fs/project}
//	    settings: {channel: drafts}
//
// # The four lifecycle sections
//
// prepare / observe / referees are the three hook-shaped sections;
// policy is a struct, not a hook. They are not interchangeable:
//
//   - prepare is a chain of [agent.Preparer]. Each Preparer takes
//     the previous link's board and returns a new one, so the chain
//     builds up: history load → recall → system prompt. The first
//     link receives a board freshly seeded with req.Message on
//     MainChannel. Any error short-circuits the run.
//
//   - observe is a list of [agent.Observer]. Observers are
//     read-only; their four methods (OnRunStart, OnInterrupt,
//     OnRunRevise, OnRunEnd) cannot affect the outcome, and panics
//     are swallowed. Use them for logging, metrics, transcript
//     persistence.
//
//   - referees is a list of [agent.Referee]. Each Referee returns
//     a [agent.Decision] which agent merges via OR over booleans
//     and first-non-empty Reason. Use them for disposition
//     (discard-on-interrupt) and quality control.
//
//   - policy is a struct, not a hook. It holds per-call harness
//     knobs (max_revise, artifact_channels) and is read by the
//     engine factory, not by a registered factory.
//
// # Ownership follows provenance
//
// A resource is constructed by [Builder.Build] and closed by
// [Result.Close], in reverse construction order so nothing outlives
// what it depends on. A source is a host-owned instance the document
// merely borrows, and is never closed here. That is the whole reason
// both exist: binding a process-wide singleton as a resource would
// hand its lifetime to a document that does not own it.
//
// This is why the scalar dep form always means a resource. Sources
// are rare and deliberately verbose:
//
//	deps:
//	  inference: infer                    # resource, whole
//	  workspace: fs/project               # resource, one item
//	  tools:     {source: host.tools}     # host-owned, borrowed
//
// # Containers and whole binding
//
// Some resources hold named items and implement [RefLookup]: a
// workspace registry's workspaces, a sandbox registry's runners.
// Those are addressed as "resource/item", and a successful Lookup
// is the validation.
//
// Others are single objects — an inference Assembly, a tool
// Assembly — because selection happens inside a call rather than
// at binding time: a model is chosen by inference.ModelRef per
// request, a tool by name per call. Those bind whole, and an item
// name on them is a build error. Only whole binding checks kind
// against the consumer's declared DepSpec.Type; addressing an item
// skips it, since there the kind names the container and the dep
// type names the item.
//
// # Dependency order
//
// Resources form a DAG, not a list: a sandbox registry needs a
// workspace registry, an inference runtime lives alongside tools.
// Build resolves construction order topologically and reports a
// cycle with the names involved. Nothing in the document declares
// order.
//
// A resource nothing binds is a build error — dead config is treated
// like a typo. Every consumer counts: another resource, an agent's
// engine dep, or a prepare/observe/referee dep.
//
// # What this package does not own
//
// Graph definitions are not resources. A graph is JSON-shaped
// (sdk/graph.GraphDefinition) and graph.Build returns a *Graph
// that IS an agent.Engine, so a graph belongs to its engine
// factory's settings, reached through engine.settings above.
//
// Running a turn is not here either. Build produces assembled
// instances; session concerns — turn lifecycle, streaming,
// interrupts, conversation state — belong to a runtime layered on
// top.
package deploy
