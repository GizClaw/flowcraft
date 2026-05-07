// Package vessel is the FlowCraft Agent runtime — the in-process
// controller that brings a [spec.Spec] to life.
//
// A Vessel bundles one or more agents, their shared memory, the
// engines that run them, and the lifecycle that supervises them all
// into a single managed entity. The package consumes the declarative
// [spec.Spec] and turns it into a running, observable,
// restartable runtime driven by the Captain controller.
//
// # Layering
//
// Vessel sits above sdk/agent + sdk/engine and orchestrates them
// using sdk/history (shared transcript), sdk/event (envelope
// routing), sdk/llm (probe-side resolution) and sdk/tool
// (allow-list propagation). It deliberately lives in its own Go
// module so that sdk/* consumers are not forced to import the
// runtime's dependency surface — applications that only need
// primitives continue to depend on sdk/* alone.
//
// # v0.1.0 capabilities
//
//   - [Captain] lifecycle: PhasePending → PhaseRunning →
//     (PhaseDraining | PhaseStopping) → PhaseStopped, surfaced via
//     [Captain.Phase] and a SubjectPhaseChanged envelope on the bus.
//   - Multi-agent dispatch: [Captain.Submit] / [Captain.Call] route
//     by agent name; one agent.Agent is assembled per
//     [spec.Agent] entry.
//   - Sidecar agents: every [spec.Agent] with Sidecar=true
//     subscribes to its declared SubscribeTo event.Pattern at
//     Launch and runs once per matching envelope.
//   - Shared history: when spec.History is set the Captain wires a
//     BoardSeeder + history-appending Observer onto every Run
//     according to [spec.HistoryAccess]; ReadOnly agents see
//     a filtered transcript via history.LoadFiltered.
//   - Sandbox: Tool allow-list (via [spec.Agent.Tools]) +
//     [spec.Resources.MaxConcurrentRuns] (semaphore) +
//     [spec.Resources.TurnTimeout] (per-Run ctx timeout).
//   - Probes: pluggable [spec.Probe] interface, built-in
//     [LLMReachableProbe], plus [spec.RestartNever] /
//     [spec.RestartOnFailure] with exponential backoff.
//   - Streaming logs: [Logs] / [LogsForRun] subscribe to
//     engine.SubjectStreamDelta envelopes the engine emits via
//     the sandbox host.
//   - Kanban agent-as-tool: when spec.Kanban is non-nil,
//     [spec.Agent.Dispatcher] gets the auto-injected
//     kanban_submit / task_context tools and the callback bridge
//     turns Card terminations into "[Task Callback]" user
//     messages on the dispatcher's history (see
//     examples/vessel-dispatcher-worker).
//
// What's deliberately deferred to v0.2.0+ : token-budget caps,
// secret/config resolution, and richer probe / restart policies.
//
// # Smallest end-to-end shape
//
//	vs := spec.Spec{
//	    Agents: []spec.Agent{{Name: "primary"}},
//	}
//	cap, err := vessel.New(spec, vessel.WithEngine(myEngine))
//	if err != nil { /* spec / option validation failed */ }
//	defer cap.Stop(ctx)
//
//	if err := cap.Launch(ctx); err != nil { /* phase conflict */ }
//
//	res, err := cap.Call(ctx, "primary", agent.Request{
//	    Message: model.NewTextMessage(model.RoleUser, "hello"),
//	})
//
// myEngine is any engine.Engine — sdk/graph/runner for production
// graphs, engine.EngineFunc for tests. See examples/vessel-basic
// and examples/vessel-multi-agent-history for runnable end-to-end
// programs.
package vessel
