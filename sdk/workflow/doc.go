// Package workflow defines the execution blackboard (Board: Vars + Channels),
// the Runtime orchestration API (Run, MemorySession, prepare/finish),
// Agent/Strategy/Memory abstractions, and Request/Result types.
//
// Graph execution Strategy lives in subpackage workflow/flowgraph (imports graph).
// Callers typically construct a Runtime with WithPrepareBoard for platform-specific
// board setup and WithDependencies for Factory + Executor when using flowgraph.
//
// Deprecated: the workflow package is superseded by the
// agent + engine + graph runtime introduced in v0.2.x and is scheduled
// for removal in v0.3.0. The breakdown below documents where each
// concept moved so callers can migrate incrementally; until v0.3.0
// every symbol in this package keeps working unchanged.
//
// Migration map (workflow → new location):
//
//   - workflow.Board / BoardSnapshot / NewBoard / RestoreBoard /
//     GetTyped / Cloneable / MainChannel
//     → engine.Board / engine.BoardSnapshot / engine.NewBoard /
//     engine.RestoreBoard / engine.GetTyped / engine.Cloneable /
//     engine.MainChannel (re-exported by sdk/graph for graph callers).
//
//   - workflow.Runtime / NewRuntime / RuntimeOption /
//     WithMemoryFactory / WithPrepareBoard / WithDependencies
//     → sdk/agent: agent.Agent + agent.Run.
//     The Runtime "prepare board → strategy.Build → run → finish"
//     pipeline is folded into agent.Run; per-platform board prep
//     becomes an agent.Seeder.
//
//   - workflow.Agent / NewAgent / AgentOption / AgentCard / Skill /
//     AgentCapabilities
//     → sdk/agent: agent.Agent (interface) + agent.New (constructor)
//     + agent.Card (descriptor). Skills are agent.Decider + agent.Tool
//     wiring on the agent value.
//
//   - workflow.Strategy / Runnable / StrategyCapabilities /
//     Dependencies / SetDep / GetDep / NewDependencies
//     → sdk/agent.Decider for the runtime selection logic;
//     graph/runner.Runner replaces the Build/Runnable split for graph
//     strategies. Dependency wiring becomes constructor arguments on
//     the concrete factory (e.g. graph/node/llmnode.Deps).
//
//   - workflow.Request / RequestConfig / NewTextRequest / MessageText
//     → sdk/agent.Request and direct use of model.Message helpers
//     (model.NewTextMessage, msg.Text()) — there is no longer a
//     separate "request text" projection.
//
//   - workflow.Result / TaskStatus / Artifact
//     → sdk/agent.Result + sdk/agent.Disposition (typed status).
//     Artifact slots are now first-class fields on agent.Result.
//
//   - workflow.Memory / MemorySession / MemoryFactory / BaseSession /
//     ContextAssembler / IncrementalSaver
//     → sdk/agent.Observer (lifecycle hooks) +
//     sdk/agent.Seeder (initial board state). History persistence is
//     no longer a runtime concern; persist agent.Result downstream.
//
//   - workflow.RunOption / WithHistory / WithStreamCallback /
//     WithMaxIterations / WithBoard / ApplyRunOpts / RunConfig
//     → executor.RunOption (graph-level) + agent.RunOption
//     (agent-level). Streaming moves to engine.Host.Publisher;
//     subscribers register at the host or via event.Bus directly.
//
//   - workflow.StreamEvent / StreamCallback
//     → event.Envelope + event.Bus. Nodes emit through
//     graph.StreamPublisher (handed via ExecutionContext); aliases
//     remain in graph/deprecated.go for one minor release.
//
//   - workflow.Task / TaskManager
//     → sdk/agent.Run (per-invocation handle).
//     There is no replacement for the global TaskManager; orchestration
//     of multiple agent runs is the host application's concern.
//
// Until v0.3.0 the agent, engine and graph packages are the sanctioned
// way to build new code; existing workflow callers continue to compile
// against the legacy API but will see staticcheck SA1019 warnings.
package workflow
